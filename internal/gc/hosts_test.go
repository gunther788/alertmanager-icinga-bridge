// SPDX-License-Identifier: BSD-3-Clause

package gc

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NETWAYS/alertmanager-icinga-bridge/internal/icinga2"
)

type forgetter struct {
	mu        sync.Mutex
	forgotten []string
}

func (f *forgetter) Forget(name string) {
	f.mu.Lock()
	f.forgotten = append(f.forgotten, name)
	f.mu.Unlock()
}

func TestCollectHosts(t *testing.T) {
	now := time.Unix(1700000000, 0)
	old := float64(now.Add(-200 * time.Hour).Unix())
	recent := float64(now.Add(-time.Hour).Unix())

	hostList := []map[string]any{
		{"name": "empty-old", "vars": map[string]any{"bridge_uuid": "unittest", "bridge_created": old}},
		{"name": "empty-recent", "vars": map[string]any{"bridge_uuid": "unittest", "bridge_created": recent}},
		{"name": "busy-old", "vars": map[string]any{"bridge_uuid": "unittest", "bridge_created": old}},
		{"name": "new-service", "vars": map[string]any{"bridge_uuid": "unittest", "bridge_created": old}},
		{"name": "foreign", "vars": map[string]any{"bridge_uuid": "other", "bridge_created": old}},
		{"name": "no-created", "vars": map[string]any{"bridge_uuid": "unittest"}},
		{"name": "unittest", "vars": map[string]any{"bridge_uuid": "unittest", "bridge_created": old}},
	}

	var (
		mu      sync.Mutex
		deleted []string
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, _ := url.PathUnescape(r.URL.EscapedPath())

		var filter icinga2.QueryFilter

		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &filter)

		results := []any{}

		switch {
		case method(r) == http.MethodGet && path == "/v1/objects/hosts/":
			if filter.Filter != `host.vars.bridge_uuid == "unittest"` {
				t.Errorf("unexpected host filter %q", filter.Filter)
			}

			for _, h := range hostList {
				results = append(results, map[string]any{"attrs": h})
			}
		case method(r) == http.MethodGet && path == "/v1/objects/services/":
			switch {
			case filter.Filter == `service.vars.bridge_uuid == "unittest"`:
				// A firing service keeps its host
				results = append(results, map[string]any{"attrs": map[string]any{"name": "svc", "host_name": "busy-old", "state": 2}})
			case strings.Contains(filter.Filter, `host.name == "new-service"`):
				results = append(results, map[string]any{"attrs": map[string]any{"name": "svc", "host_name": "new-service"}})
			}
		case r.Method == http.MethodDelete && strings.HasPrefix(path, "/v1/objects/hosts/"):
			if r.URL.Query().Get("cascade") != "1" {
				t.Errorf("host delete without cascade")
			}

			mu.Lock()
			deleted = append(deleted, strings.TrimPrefix(path, "/v1/objects/hosts/"))
			mu.Unlock()

			return
		default:
			http.NotFound(w, r)
			return
		}

		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	}))
	defer ts.Close()

	config := testConfig(ts.URL)
	config.ID = "unittest"
	config.HostKeepFor = 168 * time.Hour

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cache := &forgetter{}
	g := NewGarbageCollector(config, logger, icinga2.NewClient(config, logger)).WithHostCache(cache)
	g.now = func() time.Time { return now }

	g.collect(context.Background())

	if !slices.Equal(deleted, []string{"empty-old"}) {
		t.Fatalf("want only empty-old deleted, got %v", deleted)
	}

	if !slices.Contains(cache.forgotten, "empty-old") {
		t.Errorf("deleted host must be dropped from the cache, got %v", cache.forgotten)
	}

	// Disabled host removal
	deleted = nil
	config.HostKeepFor = 0
	g.collect(context.Background())

	if len(deleted) != 0 {
		t.Fatalf("host removal is disabled, but deleted %v", deleted)
	}
}

func TestHeartbeat_CreatesBridgeObjects(t *testing.T) {
	var (
		mu        sync.Mutex
		hostBody  map[string]any
		svcBody   map[string]any
		svcExists bool
		results   int
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		path, _ := url.PathUnescape(r.URL.EscapedPath())

		switch {
		case path == "/v1/objects/hosts/alertmanager-bridge" && method(r) == http.MethodGet:
			http.NotFound(w, r)
		case path == "/v1/objects/hosts/alertmanager-bridge" && r.Method == http.MethodPut:
			_ = json.NewDecoder(r.Body).Decode(&hostBody)
		case path == "/v1/objects/services/alertmanager-bridge!heartbeat" && method(r) == http.MethodGet:
			if !svcExists {
				http.NotFound(w, r)
				return
			}

			_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{map[string]any{"attrs": map[string]any{"name": "heartbeat", "host_name": "alertmanager-bridge"}}}})
		case path == "/v1/objects/services/alertmanager-bridge!heartbeat" && r.Method == http.MethodPut:
			_ = json.NewDecoder(r.Body).Decode(&svcBody)
			svcExists = true
		case path == "/v1/actions/process-check-result/":
			results++
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	config := testConfig(ts.URL)
	config.ID = "k8s"
	config.IcingaHostname = "alertmanager-bridge"
	config.HeartbeatInterval = time.Minute
	config.HostTemplates = []string{"generic-host"}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	g := NewGarbageCollector(config, logger, icinga2.NewClient(config, logger))

	if err := g.sendHeartbeat(context.Background()); err != nil {
		t.Fatal(err)
	}

	if vars, _ := hostBody["attrs"].(map[string]any)["vars"].(map[string]any); vars["bridge_uuid"] != "k8s" || vars["bridge_kind"] != "bridge" {
		t.Errorf("unexpected host %v", hostBody)
	}

	attrs, _ := svcBody["attrs"].(map[string]any)
	if attrs["vars.bridge_kind"] != "heartbeat" || attrs["check_interval"] != 180.0 || attrs["vars.dummy_state"] != 2.0 {
		t.Errorf("unexpected service %v", svcBody)
	}

	if results != 1 {
		t.Errorf("want 1 heartbeat result, got %d", results)
	}

	// The bridge host is never removed
	if g.hostIsRemovable(icinga2.Host{Name: "alertmanager-bridge", Vars: icinga2.Vars{"bridge_uuid": "k8s", "bridge_created": 1.0}}) {
		t.Error("the bridge host must not be removable")
	}
}

// method returns the HTTP method, honouring X-HTTP-Method-Override
func method(r *http.Request) string {
	if m := r.Header.Get("X-HTTP-Method-Override"); m != "" {
		return m
	}

	return r.Method
}
