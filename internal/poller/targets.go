// SPDX-License-Identifier: BSD-3-Clause

package poller

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Target is an Alertmanager whose active alerts are polled.
type Target struct {
	// Name identifies the target in logs and in the name of its health service;
	// default: the host of the URL.
	Name string `yaml:"name"`
	// URL is the base URL of the Alertmanager, e.g. https://alertmanager.xraydmz2.prod.example.com
	URL string `yaml:"url"`
	// Labels are set on every alert of this Alertmanager (overriding the alert's own
	// values) and identify the services that belong to it, e.g. {cluster: xraydmz2}.
	Labels map[string]string `yaml:"labels"`
	// Filter holds additional Alertmanager matchers, e.g. severity=~"warning|critical".
	Filter []string `yaml:"filter"`
	// Receiver is a regular expression for the receivers of the alerts to poll;
	// it overrides --poll-receiver.
	Receiver *string `yaml:"receiver"`
}

// TargetsFile is the format of --alertmanagers-file.
type TargetsFile struct {
	Alertmanagers []Target `yaml:"alertmanagers"`
}

// LoadTargets reads and validates a targets file.
func LoadTargets(path string) ([]Target, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("could not read Alertmanagers file: %w", err)
	}

	var file TargetsFile

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	err = dec.Decode(&file)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("could not parse Alertmanagers file %s: %w", path, err)
	}

	err = validateTargets(file.Alertmanagers)
	if err != nil {
		return nil, fmt.Errorf("invalid Alertmanagers file %s: %w", path, err)
	}

	return file.Alertmanagers, nil
}

func validateTargets(targets []Target) error {
	names := map[string]struct{}{}
	labelSets := map[string]string{}

	for i := range targets {
		t := &targets[i]

		u, err := url.Parse(t.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("alertmanager %d: url %q is not an http(s) URL", i+1, t.URL)
		}

		t.URL = strings.TrimRight(t.URL, "/")

		if t.Name == "" {
			t.Name = u.Host
		}

		if _, ok := names[t.Name]; ok {
			return fmt.Errorf("alertmanager %q is listed twice", t.Name)
		}

		names[t.Name] = struct{}{}

		if len(t.Labels) == 0 {
			return fmt.Errorf("alertmanager %q: labels are required (e.g. cluster: <name>), they identify its services", t.Name)
		}

		key := labelKey(t.Labels)
		if other, ok := labelSets[key]; ok {
			return fmt.Errorf("alertmanagers %q and %q have the same labels", other, t.Name)
		}

		labelSets[key] = t.Name
	}

	return nil
}

func labelKey(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%q,", k, labels[k])
	}

	return b.String()
}
