// SPDX-License-Identifier: BSD-3-Clause

// Package poller reads the active alerts of Alertmanagers and reconciles the
// Icinga services of the bridge with them: changed results are submitted,
// services of alerts that are gone are resolved, silences become downtimes,
// and every Alertmanager gets a service that shows whether it can be reached.
package poller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"sync"
	"time"

	"github.com/NETWAYS/alertmanager-icinga-bridge/internal/api"
	"github.com/NETWAYS/alertmanager-icinga-bridge/internal/icinga2"
)

// Vars set on the health service of an Alertmanager
const (
	// VarBridgeKind marks services that are not alerts; the garbage collection keeps them.
	VarBridgeKind = "bridge_kind"
	// KindAlertmanager is the VarBridgeKind of the health service of an Alertmanager.
	KindAlertmanager = "alertmanager"
)

const typeService = "Service"

var silenceIDPattern = regexp.MustCompile(`\(silence ([0-9A-Za-z-]+)\)`)

// Handler turns alerts into Icinga check results; implemented by api.Listener
type Handler interface {
	PlanAlert(ctx context.Context, alert *api.Alert) (api.Plan, error)
	ApplyPlan(ctx context.Context, plan api.Plan, alert *api.Alert) error
	HostForLabels(ctx context.Context, labels map[string]string) (string, bool, error)
}

// IcingaClient is the part of the Icinga client the poller needs
type IcingaClient interface {
	GetServiceStates(ctx context.Context, filter string) ([]icinga2.ServiceState, error)
	GetService(ctx context.Context, name string) (icinga2.Service, error)
	CreateService(ctx context.Context, service icinga2.Service) error
	ProcessCheckResult(ctx context.Context, action icinga2.Action) error
	GetDowntimes(ctx context.Context, filter string) ([]icinga2.Downtime, error)
	ScheduleDowntime(ctx context.Context, downtime icinga2.ScheduleDowntime) error
	RemoveDowntime(ctx context.Context, fullName string) error
}

// Config configures the poller
type Config struct {
	// ID of the bridge instance
	ID string
	// Path of the targets file
	Path string
	// Interval between polls
	Interval time.Duration
	// Filter holds Alertmanager matchers applied to every target
	Filter []string
	// Receiver is the default receiver regular expression
	Receiver string
	// ResolveAfter is the number of polls an alert has to be missing before its service is resolved
	ResolveAfter int
	// SyncSilences turns silences into downtimes
	SyncSilences bool
	// Concurrency is the number of Alertmanagers polled at the same time
	Concurrency int
	// ServiceTemplates are imported by the health services
	ServiceTemplates []string
}

// Poller polls Alertmanagers and reconciles Icinga with their alerts
type Poller struct {
	cfg     Config
	am      *AlertmanagerClient
	handler Handler
	icinga  IcingaClient
	logger  *slog.Logger
	now     func() time.Time

	mu          sync.Mutex
	targets     []Target
	missing     map[string]map[string]int
	healthKnown map[string]struct{}
}

// New returns a poller for the targets in cfg.Path
func New(cfg Config, am *AlertmanagerClient, handler Handler, icinga IcingaClient, logger *slog.Logger) (*Poller, error) {
	if cfg.ResolveAfter < 1 {
		cfg.ResolveAfter = 1
	}

	if cfg.Concurrency < 1 {
		cfg.Concurrency = 1
	}

	p := &Poller{
		cfg:         cfg,
		am:          am,
		handler:     handler,
		icinga:      icinga,
		logger:      logger,
		now:         time.Now,
		missing:     map[string]map[string]int{},
		healthKnown: map[string]struct{}{},
	}

	err := p.Reload()
	if err != nil {
		return nil, err
	}

	return p, nil
}

// Reload re-reads the targets file. On error, the previous targets are kept.
func (p *Poller) Reload() error {
	targets, err := LoadTargets(p.cfg.Path)
	if err != nil {
		return err
	}

	p.mu.Lock()
	p.targets = targets
	p.mu.Unlock()

	p.logger.Info("Loaded Alertmanagers to poll", "component", "poller", "count", len(targets), "file", p.cfg.Path)

	return nil
}

// Path returns the targets file path
func (p *Poller) Path() string {
	return p.cfg.Path
}

// Run polls until the context is cancelled
func (p *Poller) Run(ctx context.Context) {
	ticker := time.NewTicker(p.cfg.Interval)
	defer ticker.Stop()

	for {
		p.Poll(ctx)

		select {
		case <-ctx.Done():
			p.logger.Info("Poller received shutdown", "component", "poller")
			return
		case <-ticker.C:
		}
	}
}

// Poll polls all targets once
func (p *Poller) Poll(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, max(p.cfg.Interval, time.Minute))
	defer cancel()

	p.mu.Lock()
	targets := append([]Target{}, p.targets...)
	p.mu.Unlock()

	states, errStates := p.icinga.GetServiceStates(ctx, fmt.Sprintf(`service.vars.bridge_uuid == %q`, p.cfg.ID))
	if errStates != nil {
		p.logger.Error("Could not fetch services from Icinga, skipping this poll", "component", "poller", "error", errStates.Error())
		return
	}

	byName := make(map[string]*icinga2.ServiceState, len(states))
	for i := range states {
		byName[states[i].FullName()] = &states[i]
	}

	var downtimes []icinga2.Downtime

	syncSilences := p.cfg.SyncSilences
	if syncSilences {
		var errDowntimes error

		downtimes, errDowntimes = p.icinga.GetDowntimes(ctx, fmt.Sprintf(`downtime.author == %q`, p.author()))
		if errDowntimes != nil {
			p.logger.Error("Could not fetch downtimes from Icinga, not syncing silences in this poll", "component", "poller", "error", errDowntimes.Error())

			syncSilences = false
		}
	}

	sem := make(chan struct{}, p.cfg.Concurrency)

	var wg sync.WaitGroup

	for i := range targets {
		t := &targets[i]

		missing := p.missingFor(t.Name)

		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()

			r := reconciliation{p: p, t: t, states: byName, missing: missing, syncSilences: syncSilences, downtimes: downtimes}
			r.run(ctx)
		})
	}

	wg.Wait()

	p.pruneMissing(targets)
}

func (p *Poller) author() string {
	return "alertmanager-icinga-bridge/" + p.cfg.ID
}

func (p *Poller) missingFor(name string) map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()

	m, ok := p.missing[name]
	if !ok {
		m = map[string]int{}
		p.missing[name] = m
	}

	return m
}

func (p *Poller) pruneMissing(targets []Target) {
	p.mu.Lock()
	defer p.mu.Unlock()

	keep := make(map[string]struct{}, len(targets))
	for _, t := range targets {
		keep[t.Name] = struct{}{}
	}

	for name := range p.missing {
		if _, ok := keep[name]; !ok {
			delete(p.missing, name)
		}
	}
}

// reconciliation is one poll of one target
type reconciliation struct {
	p            *Poller
	t            *Target
	states       map[string]*icinga2.ServiceState
	missing      map[string]int
	syncSilences bool
	downtimes    []icinga2.Downtime

	seen     map[string]struct{}
	silenced map[string][]string // service full name -> silence IDs
	stats    struct{ alerts, silenced, submitted, unchanged, failed, resolved int }
}

func (r *reconciliation) run(ctx context.Context) {
	log := r.p.logger.With("component", "poller", "alertmanager", r.t.Name)

	receiver := r.p.cfg.Receiver
	if r.t.Receiver != nil {
		receiver = *r.t.Receiver
	}

	filter := append(append([]string{}, r.p.cfg.Filter...), r.t.Filter...)

	alerts, errAlerts := r.p.am.Alerts(ctx, r.t.URL, filter, receiver)

	r.reportHealth(ctx, log, len(alerts), countSilenced(alerts), errAlerts)

	if errAlerts != nil {
		// Without a complete list of alerts nothing can be resolved
		log.Warn("Could not poll Alertmanager", "url", r.t.URL, "error", errAlerts.Error())
		return
	}

	r.seen = make(map[string]struct{}, len(alerts))
	r.silenced = map[string][]string{}

	for i := range alerts {
		r.alert(ctx, log, &alerts[i])
	}

	r.resolveMissing(ctx, log)

	if r.syncSilences {
		r.reconcileDowntimes(ctx, log)
	}

	level := slog.LevelDebug
	if r.stats.submitted > 0 || r.stats.resolved > 0 || r.stats.failed > 0 {
		level = slog.LevelInfo
	}

	log.Log(ctx, level, "Polled Alertmanager", "alerts", r.stats.alerts, "silenced", r.stats.silenced,
		"submitted", r.stats.submitted, "unchanged", r.stats.unchanged, "resolved", r.stats.resolved, "failed", r.stats.failed)
}

// alert submits the result of one active alert unless Icinga already shows it
func (r *reconciliation) alert(ctx context.Context, log *slog.Logger, ga *GettableAlert) {
	r.stats.alerts++

	labels := maps.Clone(ga.Labels)
	if labels == nil {
		labels = map[string]string{}
	}

	maps.Copy(labels, r.t.Labels)

	alert := &api.Alert{
		Status:       "firing",
		Labels:       labels,
		Annotations:  ga.Annotations,
		StartsAt:     ga.StartsAt,
		EndsAt:       ga.EndsAt,
		GeneratorURL: ga.GeneratorURL,
		Fingerprint:  ga.Fingerprint,
	}

	plan, errPlan := r.p.handler.PlanAlert(ctx, alert)
	if errPlan != nil {
		r.stats.failed++

		log.Error("Could not handle polled alert", "alertname", labels["alertname"], "labels", labels, "error", errPlan.Error())

		return
	}

	name := plan.FullName()
	r.seen[name] = struct{}{}
	delete(r.missing, name)

	if len(ga.Status.SilencedBy) > 0 {
		r.stats.silenced++

		if !plan.HostCheck {
			r.silenced[name] = ga.Status.SilencedBy
		}
	}

	if current, ok := r.states[name]; ok && !plan.HostCheck && !plan.Heartbeat &&
		current.LastCheckResult != nil && int(current.State) == int(plan.ExitStatus) && current.Output() == plan.PluginOutput {
		r.stats.unchanged++
		return
	}

	errApply := r.p.handler.ApplyPlan(ctx, plan, alert)
	if errApply != nil {
		r.stats.failed++

		log.Error("Could not submit polled alert", "alertname", labels["alertname"], "service", name, "error", errApply.Error())

		return
	}

	r.stats.submitted++
}

// owns reports whether a service belongs to this target: an alert service whose
// label variables match the target's labels
func (r *reconciliation) owns(s *icinga2.ServiceState) bool {
	if _, ok := s.Vars[VarBridgeKind]; ok {
		return false
	}

	for k, v := range r.t.Labels {
		if value, _ := s.Vars["label_"+k].(string); value != v {
			return false
		}
	}

	return true
}

// resolveMissing sets services to OK whose alerts were not active in ResolveAfter polls
func (r *reconciliation) resolveMissing(ctx context.Context, log *slog.Logger) {
	for name, s := range r.states {
		if _, ok := r.seen[name]; ok || !r.owns(s) {
			continue
		}

		// Heartbeat services are active checks; a missing heartbeat is their job
		if _, heartbeat := s.Vars["label_heartbeat"]; heartbeat || s.State == 0 {
			delete(r.missing, name)
			continue
		}

		r.missing[name]++
		if r.missing[name] < r.p.cfg.ResolveAfter {
			continue
		}

		err := r.p.icinga.ProcessCheckResult(ctx, icinga2.Action{
			Type:         typeService,
			Filter:       fmt.Sprintf("host.name==%q && service.name==%q", s.HostName, s.Name),
			ExitStatus:   icinga2.ExitStatusOK,
			PluginOutput: "Resolved: the alert is no longer active in Alertmanager " + r.t.Name,
		})
		if err != nil {
			r.stats.failed++

			log.Error("Could not resolve service", "service", name, "error", err.Error())

			continue
		}

		delete(r.missing, name)
		r.stats.resolved++

		log.Info("Resolved service of an alert that is no longer active", "service", name)
	}
}

// reconcileDowntimes makes the downtimes of the bridge match the silences of the target's alerts
func (r *reconciliation) reconcileDowntimes(ctx context.Context, log *slog.Logger) {
	var silences map[string]Silence

	if len(r.silenced) > 0 {
		list, err := r.p.am.Silences(ctx, r.t.URL)
		if err != nil {
			log.Warn("Could not fetch silences, not syncing downtimes", "error", err.Error())
			return
		}

		silences = make(map[string]Silence, len(list))
		for _, s := range list {
			silences[s.ID] = s
		}
	}

	type key struct{ service, silence string }

	wanted := map[key]Silence{}

	for service, ids := range r.silenced {
		for _, id := range ids {
			if s, ok := silences[id]; ok && s.Status.State == "active" {
				wanted[key{service, id}] = s
			}
		}
	}

	for _, d := range r.p.relevantDowntimes(r) {
		service := d.HostName + "!" + d.ServiceName

		match := silenceIDPattern.FindStringSubmatch(d.Comment)
		if match != nil {
			if _, ok := wanted[key{service, match[1]}]; ok {
				delete(wanted, key{service, match[1]})
				continue
			}
		}

		err := r.p.icinga.RemoveDowntime(ctx, d.FullName)
		if err != nil && !errors.Is(err, icinga2.ErrNotFound) {
			log.Error("Could not remove downtime", "downtime", d.FullName, "error", err.Error())
			continue
		}

		log.Info("Removed downtime of an expired silence", "service", service, "comment", d.Comment)
	}

	now := r.p.now()

	for k, s := range wanted {
		if !s.EndsAt.After(now) {
			continue
		}

		host, service := splitName(k.service)

		err := r.p.icinga.ScheduleDowntime(ctx, icinga2.ScheduleDowntime{
			Type:      typeService,
			Filter:    fmt.Sprintf("host.name==%q && service.name==%q", host, service),
			Author:    r.p.author(),
			Comment:   fmt.Sprintf("Silenced in Alertmanager %s (silence %s) by %s: %s", r.t.Name, s.ID, s.CreatedBy, s.Comment),
			StartTime: now.Unix(),
			EndTime:   s.EndsAt.Unix(),
			Fixed:     true,
		})
		if err != nil {
			log.Error("Could not schedule downtime for silence", "service", k.service, "silence", s.ID, "error", err.Error())
			continue
		}

		log.Info("Scheduled downtime for silence", "service", k.service, "silence", s.ID, "until", s.EndsAt)
	}
}

// relevantDowntimes returns the bridge's downtimes on services of this target
func (p *Poller) relevantDowntimes(r *reconciliation) []icinga2.Downtime {
	result := []icinga2.Downtime{}

	for _, d := range r.downtimes {
		name := d.HostName + "!" + d.ServiceName

		if _, ok := r.seen[name]; ok {
			result = append(result, d)
			continue
		}

		if s, ok := r.states[name]; ok && r.owns(s) {
			result = append(result, d)
		}
	}

	return result
}

// reportHealth submits whether the Alertmanager could be polled
func (r *reconciliation) reportHealth(ctx context.Context, log *slog.Logger, alerts, silenced int, errPoll error) {
	host, derived, err := r.p.handler.HostForLabels(ctx, r.t.Labels)
	if err != nil {
		log.Error("Could not ensure host for Alertmanager health", "error", err.Error())
		return
	}

	service := "alertmanager"
	if !derived {
		service = "alertmanager-" + serviceNameChars.ReplaceAllString(r.t.Name, "_")
	}

	err = r.p.ensureHealthService(ctx, host, service, r.t)
	if err != nil {
		log.Error("Could not create Alertmanager health service", "host", host, "service", service, "error", err.Error())
		return
	}

	action := icinga2.Action{
		Type:            typeService,
		Filter:          fmt.Sprintf("host.name==%q && service.name==%q", host, service),
		ExitStatus:      icinga2.ExitStatusOK,
		PluginOutput:    fmt.Sprintf("Alertmanager %s: %d active alerts (%d silenced)", r.t.URL, alerts, silenced),
		PerformanceData: icinga2.PerfData{fmt.Sprintf("alerts=%d;;;0", alerts), fmt.Sprintf("silenced=%d;;;0", silenced)},
	}

	if errPoll != nil {
		action.ExitStatus = icinga2.ExitStatusCritical
		action.PluginOutput = fmt.Sprintf("Alertmanager %s cannot be polled: %s", r.t.URL, errPoll.Error())
		action.PerformanceData = nil
	}

	err = r.p.icinga.ProcessCheckResult(ctx, action)
	if err != nil {
		log.Error("Could not submit Alertmanager health", "host", host, "service", service, "error", err.Error())

		r.p.mu.Lock()
		delete(r.p.healthKnown, host+"!"+service)
		r.p.mu.Unlock()
	}
}

var serviceNameChars = regexp.MustCompile(`[^-+_.:,a-zA-Z0-9]`)

// ensureHealthService creates the health service of an Alertmanager if needed
func (p *Poller) ensureHealthService(ctx context.Context, host, name string, t *Target) error {
	full := host + "!" + name

	p.mu.Lock()
	_, known := p.healthKnown[full]
	p.mu.Unlock()

	if known {
		return nil
	}

	_, err := p.icinga.GetService(ctx, full)
	if errors.Is(err, icinga2.ErrNotFound) {
		// If no result arrives for three intervals, the dummy check reports UNKNOWN
		interval := (3 * p.cfg.Interval).Seconds()

		err = p.icinga.CreateService(ctx, icinga2.Service{
			Name:               name,
			HostName:           host,
			DisplayName:        "Alertmanager " + t.Name,
			CheckCommand:       "dummy",
			EnableActiveChecks: true,
			CheckInterval:      interval,
			RetryInterval:      interval,
			MaxCheckAttempts:   3,
			Templates:          p.cfg.ServiceTemplates,
			NotesURL:           t.URL,
			ActionURL:          t.URL,
			Vars: icinga2.Vars{
				"bridge_uuid":       p.cfg.ID,
				VarBridgeKind:       KindAlertmanager,
				"alertmanager_url":  t.URL,
				"alertmanager_name": t.Name,
				"dummy_state":       icinga2.ExitStatusUnknown,
				"dummy_text":        "No result from the poller of alertmanager-icinga-bridge " + p.cfg.ID,
			},
		})
		if err == nil {
			p.logger.Info("Created Alertmanager health service", "component", "poller", "service", full)
		}
	}

	if err != nil {
		return err
	}

	p.mu.Lock()
	p.healthKnown[full] = struct{}{}
	p.mu.Unlock()

	return nil
}

func countSilenced(alerts []GettableAlert) int {
	n := 0

	for _, a := range alerts {
		if len(a.Status.SilencedBy) > 0 {
			n++
		}
	}

	return n
}

func splitName(full string) (string, string) {
	for i := range len(full) {
		if full[i] == '!' {
			return full[:i], full[i+1:]
		}
	}

	return full, ""
}
