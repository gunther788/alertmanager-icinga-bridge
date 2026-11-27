// SPDX-License-Identifier: BSD-3-Clause

package poller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// GettableAlert is an alert as returned by GET /api/v2/alerts
type GettableAlert struct {
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
	Fingerprint  string            `json:"fingerprint"`
	Status       struct {
		State       string   `json:"state"`
		SilencedBy  []string `json:"silencedBy"`
		InhibitedBy []string `json:"inhibitedBy"`
	} `json:"status"`
}

// Silence is a silence as returned by GET /api/v2/silences
type Silence struct {
	ID        string    `json:"id"`
	CreatedBy string    `json:"createdBy"`
	Comment   string    `json:"comment"`
	StartsAt  time.Time `json:"startsAt"`
	EndsAt    time.Time `json:"endsAt"`
	Status    struct {
		State string `json:"state"`
	} `json:"status"`
}

// maximum size of an Alertmanager API response
const maxResponseBytes = 64 << 20

// AlertmanagerClient reads alerts and silences from the Alertmanager API v2
type AlertmanagerClient struct {
	HTTP *http.Client
}

// Alerts returns the active (including silenced, but not inhibited) alerts
func (c *AlertmanagerClient) Alerts(ctx context.Context, base string, filter []string, receiver string) ([]GettableAlert, error) {
	query := url.Values{
		"active":      {"true"},
		"silenced":    {"true"},
		"inhibited":   {"false"},
		"unprocessed": {"false"},
	}

	for _, f := range filter {
		query.Add("filter", f)
	}

	if receiver != "" {
		query.Set("receiver", receiver)
	}

	var alerts []GettableAlert

	err := c.get(ctx, base+"/api/v2/alerts?"+query.Encode(), &alerts)

	return alerts, err
}

// Silences returns all silences
func (c *AlertmanagerClient) Silences(ctx context.Context, base string) ([]Silence, error) {
	var silences []Silence

	err := c.get(ctx, base+"/api/v2/silences", &silences)

	return silences, err
}

func (c *AlertmanagerClient) get(ctx context.Context, u string, result any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "alertmanager-icinga-bridge")

	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}

	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 1024))
		return fmt.Errorf("HTTP %d from %s: %s", res.StatusCode, req.URL.Redacted(), strings.TrimSpace(string(body)))
	}

	err = json.NewDecoder(io.LimitReader(res.Body, maxResponseBytes)).Decode(result)
	if err != nil {
		return fmt.Errorf("could not decode response from %s: %w", req.URL.Redacted(), err)
	}

	return nil
}
