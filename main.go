// SPDX-License-Identifier: BSD-3-Clause

// Package main parses the CLI flags and starts the various components
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/NETWAYS/alertmanager-icinga-bridge/internal/api"
	"github.com/NETWAYS/alertmanager-icinga-bridge/internal/config"
	"github.com/NETWAYS/alertmanager-icinga-bridge/internal/gc"
	"github.com/NETWAYS/alertmanager-icinga-bridge/internal/hosts"
	"github.com/NETWAYS/alertmanager-icinga-bridge/internal/icinga2"
	"github.com/NETWAYS/alertmanager-icinga-bridge/internal/poller"

	"github.com/alecthomas/kong"
)

var (
	// These get filled at build time with the proper values.
	version = "development"
	commit  = "HEAD"
)

// buildVersion creates a string that contains the executable's version
func buildVersion() string {
	result := version

	if commit != "" {
		result = fmt.Sprintf("%s\ncommit: %s", result, commit)
	}

	return result
}

func main() {
	var cli config.CLI
	// Create and parse CLI flags
	kong.Parse(&cli,
		kong.Name("alertmanager-icinga-bridge"),
		kong.Description(`The Alertmanager to Icinga bridge can receive alerts from the Prometheus Alertmanager's generic webhook receiver and creates Icinga Services for these alerts.`),
		kong.Vars{"version": buildVersion()},
	)

	// Central logger that we pass to the components
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cli.Loglevel}))
	slog.SetDefault(logger)

	cfg, errConfig := config.NewConfigFromCLI(&cli)

	if errConfig != nil {
		logger.Error("Could not load configuration", "component", "main", "error", errConfig.Error())
		os.Exit(1)
	}

	// Derive hosts from alert labels and create them on demand
	resolver, errResolver := hosts.NewResolver(cfg.HostLabels, cfg.HostLabelSeparator, cfg.HostNamePrefix, cfg.HostMappingFile)
	if errResolver != nil {
		logger.Error("Could not load host mapping", "component", "main", "error", errResolver.Error())
		os.Exit(1)
	}

	// Create a central context to propagate a shutdown
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Create Icinga Client
	icingaClient := icinga2.NewClient(cfg, logger)

	logger.Info("Starting alertmanager-icinga-bridge", "version", version, "commit", commit, "component", "main")

	hostManager := hosts.NewManager(hosts.ManagerConfig{
		ID:           cfg.ID,
		Templates:    cfg.HostTemplates,
		CheckCommand: cfg.HostCheckCommand,
		Zone:         cfg.HostZone,
		StaticVars:   cfg.StaticHostVars,
		CacheTTL:     cfg.HostCacheTTL,
	}, icingaClient, logger)

	if resolver.Enabled() {
		logger.Info("Deriving hosts from alert labels", "component", "main", "labels", cfg.HostLabels, "mapping", cfg.HostMappingFile)
	}

	// Create and start the Service Garbage Collector
	garbagecol := gc.NewGarbageCollector(cfg, logger, icingaClient).WithHostCache(hostManager)
	garbagecol.Start(ctx)

	// Create and start the API Listener
	listener := api.NewListener(cfg, logger, icingaClient).WithHosts(resolver, hostManager)

	reloaders := []reloader{}
	if resolver.Path() != "" {
		reloaders = append(reloaders, resolver)
	}

	// Poll Alertmanagers and reconcile Icinga with their active alerts
	if cfg.AlertmanagersFile != "" {
		alertmanagers := &poller.AlertmanagerClient{HTTP: &http.Client{
			Timeout:   20 * time.Second,
			Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: cfg.PollTLSConfig},
		}}

		poll, errPoller := poller.New(poller.Config{
			ID:               cfg.ID,
			Path:             cfg.AlertmanagersFile,
			Interval:         cfg.PollInterval,
			Filter:           cfg.PollFilter,
			Receiver:         cfg.PollReceiver,
			ResolveAfter:     cfg.PollResolveAfter,
			SyncSilences:     cfg.PollSilences,
			Concurrency:      cfg.PollConcurrency,
			ServiceTemplates: cfg.IcingaTemplates,
		}, alertmanagers, listener, icingaClient, logger)
		if errPoller != nil {
			logger.Error("Could not load Alertmanagers to poll", "component", "main", "error", errPoller.Error())
			os.Exit(1) //nolint: gocritic
		}

		reloaders = append(reloaders, poll)

		go poll.Run(ctx)
	}

	go reloadOnHangup(ctx, logger, reloaders)

	//nolint: noinlineerr
	if err := listener.Run(ctx); err != nil {
		logger.Error("Listener has finished with an error", "component", "main", "error", err.Error())
	} else {
		logger.Info("Listener has finished", "component", "main")
	}
}

// reloader is a configuration file that can be re-read
type reloader interface {
	Reload() error
	Path() string
}

// reloadOnHangup re-reads the host mapping and Alertmanagers files whenever the process receives SIGHUP
func reloadOnHangup(ctx context.Context, logger *slog.Logger, reloaders []reloader) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)

	defer signal.Stop(hup)

	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			if len(reloaders) == 0 {
				logger.Info("Received SIGHUP, but there are no files to reload", "component", "main")
			}

			for _, r := range reloaders {
				errReload := r.Reload()
				if errReload != nil {
					logger.Error("Could not reload, keeping the previous configuration", "component", "main", "file", r.Path(), "error", errReload.Error())
					continue
				}

				logger.Info("Reloaded", "component", "main", "file", r.Path())
			}
		}
	}
}
