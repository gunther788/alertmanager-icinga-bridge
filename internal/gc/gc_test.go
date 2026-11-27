// SPDX-License-Identifier: BSD-3-Clause

package gc

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/NETWAYS/alertmanager-icinga-bridge/internal/config"
	"github.com/NETWAYS/alertmanager-icinga-bridge/internal/icinga2"
)

func testConfig(url string) *config.Config {
	return &config.Config{
		IcingaURL:        []string{url},
		IcingaHostname:   "unittest",
		IcingaPassword:   "password",
		IcingaUser:       "username",
		HeartbeatService: "heartbeat",
	}
}

func testServerForDelete() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/objects/services/unittest!svc":
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestGCRemoveService_WithRemoved(t *testing.T) {
	ts := testServerForDelete()
	defer ts.Close()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	config := testConfig(ts.URL)

	icingaClient := icinga2.NewClient(config, logger)

	gc := NewGarbageCollector(config, logger, icingaClient)

	svc := icinga2.Service{
		HostName: "unittest",
		Name:     "svc",
		Vars: icinga2.Vars{
			"keep_for": 123.456,
		},
	}

	_, actualErr := gc.removeServiceIfRequired(context.Background(), svc)

	if actualErr != nil {
		t.Errorf("expected no error got %v", actualErr)
	}

	actual := buf.String()
	expected := "\"Successfully removed service from Icinga\" component=gc service=unittest!svc"

	if !strings.Contains(actual, expected) {
		t.Fatalf("expected %v, got %v", expected, actual)
	}
}

func TestGCRemoveService_WithHeartbeatNoDowntime(t *testing.T) {
	ts := testServerForDelete()
	defer ts.Close()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	config := testConfig(ts.URL)

	icingaClient := icinga2.NewClient(config, logger)

	gc := NewGarbageCollector(config, logger, icingaClient)

	svc := icinga2.Service{
		HostName: "unittest",
		Name:     "svc",
		Vars: icinga2.Vars{
			"keep_for":        20.0,
			"label_heartbeat": "300s",
		},
		LastStateChange: 1770000000.0,
	}

	_, actualErr := gc.removeServiceIfRequired(context.Background(), svc)

	if actualErr != nil {
		t.Errorf("expected no error got %v", actualErr)
	}

	actual := buf.String()
	expected := "Skipping heartbeat and not downtimed service"

	if !strings.Contains(actual, expected) {
		t.Fatalf("expected:\n %v, got:\n %v", expected, actual)
	}
}

func TestGCRemoveService_WithHeartbeatDowntime(t *testing.T) {
	ts := testServerForDelete()
	defer ts.Close()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	config := testConfig(ts.URL)

	icingaClient := icinga2.NewClient(config, logger)

	gc := NewGarbageCollector(config, logger, icingaClient)

	svc := icinga2.Service{
		HostName: "unittest",
		Name:     "svc",
		Vars: icinga2.Vars{
			"keep_for":        20.0,
			"label_heartbeat": "300s",
		},
		LastStateChange: 1770000000.0,
		DowntimeDepth:   1,
	}

	_, actualErr := gc.removeServiceIfRequired(context.Background(), svc)

	if actualErr != nil {
		t.Errorf("expected no error got %v", actualErr)
	}

	actual := buf.String()
	expected := "Deleting service at Icinga API"

	if !strings.Contains(actual, expected) {
		t.Fatalf("expected:\n %v, got:\n %v", expected, actual)
	}
}

func TestGCRemoveService_WithSkippedNotOK(t *testing.T) {
	ts := testServerForDelete()
	defer ts.Close()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	config := testConfig(ts.URL)

	icingaClient := icinga2.NewClient(config, logger)

	gc := NewGarbageCollector(config, logger, icingaClient)

	svc := icinga2.Service{
		HostName: "unittest",
		Name:     "svc",
		State:    2,
		Vars: icinga2.Vars{
			"keep_for": 20.0,
		},
		LastStateChange: 1770000000.0,
	}

	_, actualErr := gc.removeServiceIfRequired(context.Background(), svc)

	if actualErr != nil {
		t.Errorf("expected no error got %v", actualErr)
	}

	actual := buf.String()
	expected := "Skipping not-OK services that are not heartbeats"

	if !strings.Contains(actual, expected) {
		t.Fatalf("expected %v, got %v", expected, actual)
	}
}

func TestGCRemoveService_WithNoKeepFor(t *testing.T) {
	ts := testServerForDelete()
	defer ts.Close()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	config := testConfig(ts.URL)

	icingaClient := icinga2.NewClient(config, logger)

	gc := NewGarbageCollector(config, logger, icingaClient)

	svc := icinga2.Service{
		HostName:        "unittest",
		Name:            "svc",
		State:           0,
		LastStateChange: 1770000000.0,
	}

	_, actualErr := gc.removeServiceIfRequired(context.Background(), svc)

	if actualErr != nil {
		t.Errorf("expected no error got %v", actualErr)
	}

	actual := buf.String()
	expected := "Deleting service at Icinga API"

	if !strings.Contains(actual, expected) {
		t.Fatalf("expected %v, got %v", expected, actual)
	}
}

func TestGCRemoveService_WithInvalidKeepFor(t *testing.T) {
	ts := testServerForDelete()
	defer ts.Close()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	config := testConfig(ts.URL)

	icingaClient := icinga2.NewClient(config, logger)

	gc := NewGarbageCollector(config, logger, icingaClient)

	svc := icinga2.Service{
		HostName: "unittest",
		Name:     "svc",
		State:    0,
		Vars: icinga2.Vars{
			"keep_for": "FOOBAR",
		},
		LastStateChange: 1770000000.0,
	}

	_, actualErr := gc.removeServiceIfRequired(context.Background(), svc)

	if actualErr != nil {
		t.Errorf("expected no error got %v", actualErr)
	}

	actual := buf.String()
	expected := "Deleting service at Icinga API"

	if !strings.Contains(actual, expected) {
		t.Fatalf("expected %v, got %v", expected, actual)
	}
}

func TestGCRemoveService_KeepsNonAlertServices(t *testing.T) {
	ts := testServerForDelete()
	defer ts.Close()

	config := testConfig(ts.URL)
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	gc := NewGarbageCollector(config, logger, icinga2.NewClient(config, logger))

	removed, err := gc.removeServiceIfRequired(context.Background(), icinga2.Service{
		HostName: "unittest",
		Name:     "svc",
		Vars:     icinga2.Vars{"keep_for": 1.0, "bridge_kind": "alertmanager"},
	})

	if removed || err != nil {
		t.Fatalf("the health service must be kept, got removed=%v err=%v", removed, err)
	}
}

func TestKeepForDuration(t *testing.T) {
	for val, want := range map[float64]time.Duration{
		604800:          168 * time.Hour, // seconds
		604800000000000: 168 * time.Hour, // nanoseconds, written up to 0.3.1
		1.5:             1500 * time.Millisecond,
		0:               0,
	} {
		if got := keepForDuration(val); got != want {
			t.Errorf("%v: want %v, got %v", val, want, got)
		}
	}

	recent := icinga2.Service{LastStateChange: float64(time.Now().Add(-time.Hour).Unix())}

	recent.Vars = icinga2.Vars{"keep_for": 604800.0}
	if !isSkipableDueToAge(recent, 0) {
		t.Error("a service OK for an hour must be kept for 168h (seconds)")
	}

	recent.Vars = icinga2.Vars{"keep_for": 604800000000000.0}
	if !isSkipableDueToAge(recent, 0) {
		t.Error("a service OK for an hour must be kept for 168h (nanoseconds)")
	}

	recent.Vars = icinga2.Vars{"keep_for": 60.0}
	if isSkipableDueToAge(recent, 0) {
		t.Error("a service OK for an hour must not be kept for 60s")
	}
}
