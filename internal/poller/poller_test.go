// SPDX-License-Identifier: BSD-3-Clause

package poller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NETWAYS/alertmanager-icinga-bridge/internal/api"
	"github.com/NETWAYS/alertmanager-icinga-bridge/internal/config"
	"github.com/NETWAYS/alertmanager-icinga-bridge/internal/hosts"
	"github.com/NETWAYS/alertmanager-icinga-bridge/internal/icinga2"
)

var filterPattern = regexp.MustCompile(`^host\.name=="([^"]*)" && service\.name=="([^"]*)"$`)

// fakeIcinga keeps hosts, services, check results and downtimes in memory
type fakeIcinga struct {
	mu        sync.Mutex
	hosts     map[string]bool
	services  map[string]map[string]any
	downtimes map[string]icinga2.Downtime
	results   map[string]int
	nextID    int
}

func newFakeIcinga() *fakeIcinga {
	return &fakeIcinga{
		hosts:     map[string]bool{"bridge": true},
		services:  map[string]map[string]any{},
		downtimes: map[string]icinga2.Downtime{},
		results:   map[string]int{},
	}
}

func (f *fakeIcinga) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	path, _ := url.PathUnescape(r.URL.EscapedPath())

	var body map[string]any

	data, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(data, &body)

	switch {
	case path == "/v1/objects/services/" && r.Method == http.MethodGet:
		results := []any{}
		for _, attrs := range f.services {
			results = append(results, map[string]any{"attrs": attrs})
		}

		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	case path == "/v1/objects/downtimes/":
		results := []any{}
		for _, d := range f.downtimes {
			results = append(results, map[string]any{"attrs": d})
		}

		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	case strings.HasPrefix(path, "/v1/objects/hosts/"):
		name := strings.TrimPrefix(path, "/v1/objects/hosts/")

		switch r.Method {
		case http.MethodGet:
			if !f.hosts[name] {
				http.NotFound(w, r)
				return
			}

			_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{map[string]any{"attrs": map[string]any{"name": name}}}})
		case http.MethodPut:
			f.hosts[name] = true
		}
	case strings.HasPrefix(path, "/v1/objects/services/"):
		f.service(w, r, strings.TrimPrefix(path, "/v1/objects/services/"), body)
	case path == "/v1/actions/process-check-result/":
		f.processCheckResult(w, body)
	case path == "/v1/actions/schedule-downtime/":
		m := filterPattern.FindStringSubmatch(body["filter"].(string))
		f.nextID++
		name := fmt.Sprintf("dt%d", f.nextID)
		f.downtimes[m[1]+"!"+m[2]+"!"+name] = icinga2.Downtime{
			FullName: m[1] + "!" + m[2] + "!" + name, Name: name, HostName: m[1], ServiceName: m[2],
			Author: body["author"].(string), Comment: body["comment"].(string), EndTime: body["end_time"].(float64),
		}
	case path == "/v1/actions/remove-downtime/":
		delete(f.downtimes, body["downtime"].(string))
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeIcinga) service(w http.ResponseWriter, r *http.Request, name string, body map[string]any) {
	switch r.Method {
	case http.MethodGet:
		attrs, ok := f.services[name]
		if !ok {
			http.NotFound(w, r)
			return
		}

		_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{map[string]any{"attrs": attrs}}})
	case http.MethodPut, http.MethodPost:
		host, short, _ := strings.Cut(name, "!")
		if !f.hosts[host] {
			http.Error(w, "host does not exist", http.StatusInternalServerError)
			return
		}

		attrs, ok := f.services[name]
		if !ok {
			attrs = map[string]any{"name": short, "host_name": host, "state": 0.0, "vars": map[string]any{}}
			f.services[name] = attrs
		}

		newAttrs, _ := body["attrs"].(map[string]any)
		for k, v := range newAttrs {
			if varName, isVar := strings.CutPrefix(k, "vars."); isVar {
				attrs["vars"].(map[string]any)[varName] = v
			}
		}
	}
}

func (f *fakeIcinga) processCheckResult(w http.ResponseWriter, body map[string]any) {
	m := filterPattern.FindStringSubmatch(body["filter"].(string))
	if m == nil {
		http.Error(w, "unsupported filter", http.StatusBadRequest)
		return
	}

	name := m[1] + "!" + m[2]

	attrs, ok := f.services[name]
	if !ok {
		http.NotFound(w, nil)
		return
	}

	attrs["state"] = body["exit_status"]
	attrs["last_check_result"] = map[string]any{"exit_status": body["exit_status"], "output": body["plugin_output"]}
	f.results[name]++
}

// fakeAlertmanager serves a configurable list of alerts and silences
type fakeAlertmanager struct {
	mu       sync.Mutex
	alerts   []GettableAlert
	silences []Silence
	down     bool
	queries  []url.Values
}

func (a *fakeAlertmanager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.down {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}

	switch r.URL.Path {
	case "/api/v2/alerts":
		a.queries = append(a.queries, r.URL.Query())
		_ = json.NewEncoder(w).Encode(a.alerts)
	case "/api/v2/silences":
		_ = json.NewEncoder(w).Encode(a.silences)
	default:
		http.NotFound(w, r)
	}
}

func amAlert(alertname, severity string, silencedBy ...string) GettableAlert {
	a := GettableAlert{
		Labels:      map[string]string{"alertname": alertname, "severity": severity, "namespace": "default"},
		Annotations: map[string]string{"message": alertname + " fires"},
	}
	a.Status.State = "active"
	a.Status.SilencedBy = silencedBy

	return a
}

type env struct {
	icinga *fakeIcinga
	am     *fakeAlertmanager
	poller *Poller
}

func setup(t *testing.T) *env {
	t.Helper()

	icinga := newFakeIcinga()
	am := &fakeAlertmanager{}

	icingaServer := httptest.NewServer(icinga)
	amServer := httptest.NewServer(am)

	t.Cleanup(icingaServer.Close)
	t.Cleanup(amServer.Close)

	path := filepath.Join(t.TempDir(), "alertmanagers.yaml")
	content := fmt.Sprintf("alertmanagers:\n  - name: xraydmz2\n    url: %s/\n    labels:\n      cluster: xraydmz2\n    filter:\n      - namespace=\"default\"\n", amServer.URL)

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{
		IcingaURL:               []string{icingaServer.URL},
		ID:                      "unittest",
		IcingaHostname:          "bridge",
		PluginOutputAnnotations: []string{"message"},
		IcingaHostObjectLabels:  []string{"icinga_use_host"},
		IcingaHostCheckLabels:   []string{"icinga_use_host_check"},
	}

	client := icinga2.NewClient(cfg, logger)
	resolver, _ := hosts.NewResolver([]string{"cluster", "project"}, "-", "", "")
	manager := hosts.NewManager(hosts.ManagerConfig{ID: "unittest", CacheTTL: time.Minute}, client, logger)
	listener := api.NewListener(cfg, logger, client).WithHosts(resolver, manager)

	p, err := New(Config{
		ID:           "unittest",
		Path:         path,
		Interval:     time.Minute,
		Filter:       []string{`severity=~"warning|critical"`},
		ResolveAfter: 2,
		SyncSilences: true,
		Concurrency:  2,
	}, &AlertmanagerClient{HTTP: amServer.Client()}, listener, client, logger)
	if err != nil {
		t.Fatal(err)
	}

	p.now = func() time.Time { return time.Unix(1700000000, 0) }

	return &env{icinga: icinga, am: am, poller: p}
}

// alertServices returns the alert services by alertname
func (e *env) alertServices(t *testing.T) map[string]map[string]any {
	t.Helper()

	e.icinga.mu.Lock()
	defer e.icinga.mu.Unlock()

	result := map[string]map[string]any{}

	for name, attrs := range e.icinga.services {
		vars, _ := attrs["vars"].(map[string]any)
		if alertname, ok := vars["label_alertname"].(string); ok {
			result[alertname] = attrs
			result[alertname]["full_name"] = name
		}
	}

	return result
}

func (e *env) results(name string) int {
	e.icinga.mu.Lock()
	defer e.icinga.mu.Unlock()

	return e.icinga.results[name]
}

func (e *env) health(t *testing.T) (float64, string) {
	t.Helper()

	e.icinga.mu.Lock()
	defer e.icinga.mu.Unlock()

	attrs, ok := e.icinga.services["xraydmz2!alertmanager"]
	if !ok {
		t.Fatalf("no health service; services: %v", e.icinga.services)
	}

	cr, _ := attrs["last_check_result"].(map[string]any)

	return attrs["state"].(float64), fmt.Sprint(cr["output"])
}

func TestPoll(t *testing.T) {
	e := setup(t)
	ctx := context.Background()

	e.am.alerts = []GettableAlert{
		amAlert("PodCrashLooping", "critical"),
		amAlert("PodPending", "warning", "silence-1"),
	}
	// The target's labels override the alert's own
	e.am.alerts[0].Labels["cluster"] = "wrong"
	e.am.silences = []Silence{{ID: "silence-1", CreatedBy: "alice", Comment: "maintenance", EndsAt: time.Unix(1700003600, 0)}}
	e.am.silences[0].Status.State = "active"

	e.poller.Poll(ctx)

	q := e.am.queries[0]
	if q.Get("active") != "true" || q.Get("silenced") != "true" || q.Get("inhibited") != "false" ||
		strings.Join(q["filter"], " ") != `severity=~"warning|critical" namespace="default"` {
		t.Errorf("unexpected query %v", q)
	}

	services := e.alertServices(t)
	if len(services) != 2 {
		t.Fatalf("want 2 alert services, got %v", services)
	}

	crash := services["PodCrashLooping"]
	if crash["host_name"] != "xraydmz2" || crash["state"] != 2.0 {
		t.Errorf("unexpected PodCrashLooping service %v", crash)
	}

	if services["PodPending"]["state"] != 1.0 {
		t.Errorf("unexpected PodPending service %v", services["PodPending"])
	}

	if state, output := e.health(t); state != 0 || !strings.Contains(output, "2 active alerts (1 silenced)") {
		t.Errorf("unexpected health %v %q", state, output)
	}

	if len(e.icinga.downtimes) != 1 {
		t.Fatalf("want 1 downtime, got %v", e.icinga.downtimes)
	}

	for _, d := range e.icinga.downtimes {
		if d.Author != "alertmanager-icinga-bridge/unittest" || d.EndTime != 1700003600 ||
			!strings.Contains(d.Comment, "(silence silence-1) by alice: maintenance") ||
			d.HostName+"!"+d.ServiceName != services["PodPending"]["full_name"] {
			t.Errorf("unexpected downtime %+v", d)
		}
	}

	crashName := crash["full_name"].(string)
	pendingName := services["PodPending"]["full_name"].(string)

	// Nothing changed: no new results for the alert services, no new downtimes
	e.poller.Poll(ctx)

	if e.results(crashName) != 1 || e.results(pendingName) != 1 {
		t.Errorf("unchanged alerts must not be resubmitted, got %d and %d", e.results(crashName), e.results(pendingName))
	}

	if len(e.icinga.downtimes) != 1 {
		t.Errorf("want 1 downtime, got %v", e.icinga.downtimes)
	}

	// PodCrashLooping is gone, the silence expired
	e.am.alerts = []GettableAlert{amAlert("PodPending", "warning")}
	e.am.silences = nil

	e.poller.Poll(ctx)

	if len(e.icinga.downtimes) != 0 {
		t.Errorf("the downtime of the expired silence must be removed, got %v", e.icinga.downtimes)
	}

	if state := e.alertServices(t)["PodCrashLooping"]["state"]; state != 2.0 {
		t.Errorf("must not resolve after one missing poll, got state %v", state)
	}

	e.poller.Poll(ctx)

	crash = e.alertServices(t)["PodCrashLooping"]
	if crash["state"] != 0.0 || !strings.Contains(fmt.Sprint(crash["last_check_result"]), "no longer active") {
		t.Errorf("must be resolved after two missing polls, got %v", crash)
	}
}

func TestPoll_AlertmanagerDown(t *testing.T) {
	e := setup(t)
	ctx := context.Background()

	e.am.alerts = []GettableAlert{amAlert("PodCrashLooping", "critical")}
	e.poller.Poll(ctx)

	e.am.down = true

	for range 3 {
		e.poller.Poll(ctx)
	}

	if state := e.alertServices(t)["PodCrashLooping"]["state"]; state != 2.0 {
		t.Errorf("nothing may be resolved while the Alertmanager is down, got state %v", state)
	}

	if state, output := e.health(t); state != 2 || !strings.Contains(output, "cannot be polled") {
		t.Errorf("unexpected health %v %q", state, output)
	}

	e.icinga.mu.Lock()
	vars := e.icinga.services["xraydmz2!alertmanager"]["vars"].(map[string]any)
	e.icinga.mu.Unlock()

	if vars["bridge_kind"] != "alertmanager" || vars["dummy_state"] != 3.0 {
		t.Errorf("unexpected health service vars %v", vars)
	}
}

func TestLoadTargets(t *testing.T) {
	testCases := map[string]struct {
		content string
		wantErr string
	}{
		"ok":             {content: "alertmanagers:\n  - url: https://am.example.com/\n    labels: {cluster: a}\n"},
		"no labels":      {content: "alertmanagers:\n  - url: https://am.example.com\n", wantErr: "labels are required"},
		"bad url":        {content: "alertmanagers:\n  - url: am.example.com\n    labels: {cluster: a}\n", wantErr: "not an http(s) URL"},
		"duplicate name": {content: "alertmanagers:\n  - {name: a, url: 'https://x', labels: {cluster: a}}\n  - {name: a, url: 'https://y', labels: {cluster: b}}\n", wantErr: "listed twice"},
		"same labels":    {content: "alertmanagers:\n  - {url: 'https://x', labels: {cluster: a}}\n  - {url: 'https://y', labels: {cluster: a}}\n", wantErr: "same labels"},
		"unknown field":  {content: "alertmanagers:\n  - {url: 'https://x', labels: {cluster: a}, lables: {}}\n", wantErr: "field lables not found"},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "alertmanagers.yaml")
			_ = os.WriteFile(path, []byte(tc.content), 0o600)

			targets, err := LoadTargets(path)
			if tc.wantErr == "" {
				if err != nil || targets[0].Name != "am.example.com" || targets[0].URL != "https://am.example.com" {
					t.Fatalf("unexpected %v %v", targets, err)
				}

				return
			}

			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}
