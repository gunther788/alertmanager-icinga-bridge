# Alertmanager-Icinga-Bridge

One bridge brings the alerts of many Prometheus Alertmanagers (e.g. one per Kubernetes cluster) into
Icinga 2. Every alert becomes an Icinga service on a host derived from the alert's `cluster` and
`project` labels; the hosts are created on demand. The bridge polls the Alertmanagers' API to keep Icinga
in sync with their active alerts and silences, and/or receives their notifications on a webhook.

This is a fork of [NETWAYS/alertmanager-icinga-bridge](https://github.com/NETWAYS/alertmanager-icinga-bridge),
itself a fork of [vshn/signalilo](https://github.com/vshn/signalilo).

## Overview

| Alert labels | Icinga host |
| ------------ | ----------- |
| `cluster=xraydmz2`, `project=p-q2v7w` | `xraydmz2-p-q2v7w` |
| the same, with the mapping `xraydmz2/p-q2v7w: system` | `xraydmz2-system` |
| `cluster=xraydmz2` (no `project`, e.g. `Watchdog`) | `xraydmz2` |
| no `cluster` label (webhook only) | `alertmanager-bridge` |
| `icinga_use_host=MyHost` | `MyHost` (must exist; takes precedence) |

The bridge creates and maintains in Icinga:

* **`alertmanager-bridge`** (`--icinga-hostname`): the bridge's own host, with the service `heartbeat`
  that turns CRITICAL when the bridge stops. Alerts without host labels land here.
* **A host per cluster and project** with a service per alert (state from the `severity` label).
* **`<cluster>!alertmanager`** (with polling): whether the cluster's Alertmanager can be polled.
* **Downtimes** for silenced alerts (with polling).

Everything it creates carries `vars.bridge_uuid = <--id>`; services and hosts that are no longer needed
are removed again.

## Installation

### RPM

[`alertmanager-icinga-bridge.spec`](alertmanager-icinga-bridge.spec) builds the package; the GitLab
pipeline ([`.gitlab-ci.yml`](.gitlab-ci.yml)) only includes the shared mock template
(`ops/packages/templates`, `.gitlab-ci-mock.yml`), which builds the source RPM and the RPM with mock
(currently for `epel-9`) and publishes them to the repository. The GitLab project has to be named
`alertmanager-icinga-bridge`, as the template looks for `${CI_PROJECT_NAME}.spec`.

`mock --buildsrpm --sources=<checkout>` copies the checkout into the SOURCES directory; the spec packs it
into its source tarball there (with the commit, shown by `--version`). Go modules and, as EL9's Go is older
than `go.mod` requires, the Go toolchain are downloaded during the build (mock runs with `--enable-network`;
set `GOPROXY`/`GOSUMDB` for a private proxy).

The spec's `Version` and `Release` are the package version: bump them for every change that should reach
the repository, as each pipeline builds whatever the spec says.

To build by hand from a checkout:

```bash
dnf install rpm-build systemd-rpm-macros golang
rpmbuild -ba --define "_sourcedir $PWD" alertmanager-icinga-bridge.spec
```

The package installs:

* `/usr/bin/alertmanager-icinga-bridge`
* `alertmanager-icinga-bridge.service`, running as the `alertmanager-icinga-bridge` user;
  `systemctl reload` re-reads the host mapping and Alertmanagers files
* `/etc/alertmanager-icinga-bridge/` (group `alertmanager-icinga-bridge`) with
  * `environment` (mode 0600): the settings as environment variables
  * empty `host-mapping.yml` and `alertmanagers.yml`
  * put the webhook TLS certificate and key here, too
* the example Icinga configuration and example files under `/usr/share/doc/alertmanager-icinga-bridge`

```bash
dnf install alertmanager-icinga-bridge
vi /etc/alertmanager-icinga-bridge/environment /etc/alertmanager-icinga-bridge/alertmanagers.yml
systemctl enable --now alertmanager-icinga-bridge
```

### Container

`make build-image` builds a container image from the [`Dockerfile`](Dockerfile); configure it with the
same environment variables.

## Quick start

1. Load [`contrib/icinga2-example.conf`](contrib/icinga2-example.conf) into Icinga (templates, host
   groups and the API user) and adjust `k8s` (the bridge's `--id`) and the password.
2. List the Alertmanagers in `/etc/alertmanager-icinga-bridge/alertmanagers.yml`
   (see [Polling Alertmanagers](#polling-alertmanagers)).
3. Set at least these in `/etc/alertmanager-icinga-bridge/environment`:

   ```
   ALERTMANAGER_ICINGA_BRIDGE_ID=k8s
   ALERTMANAGER_ICINGA_BRIDGE_ICINGA_URL=https://localhost:5665
   ALERTMANAGER_ICINGA_BRIDGE_ICINGA_USERNAME=alertmanager-bridge
   ALERTMANAGER_ICINGA_BRIDGE_ICINGA_PASSWORD='CHANGEME'
   ALERTMANAGER_ICINGA_BRIDGE_ICINGA_CA=/var/lib/icinga2/certs/ca.crt
   ALERTMANAGER_ICINGA_BRIDGE_HOST_TEMPLATE=alertmanager-host
   ALERTMANAGER_ICINGA_BRIDGE_SERVICE_TEMPLATE=alertmanager-service
   ALERTMANAGER_ICINGA_BRIDGE_ALERTMANAGERS_FILE=/etc/alertmanager-icinga-bridge/alertmanagers.yml
   ALERTMANAGER_ICINGA_BRIDGE_POLL_FILTER='severity=~"warning|critical"'
   ```

   systemd strips quotes and interprets backslashes in this file; quote values with special characters
   in single quotes.
4. `systemctl enable --now alertmanager-icinga-bridge`

The same settings are available as command line flags (see [Configuration](#configuration)).

## Hosts from alert labels

The values of the `--host-labels` (default `cluster,project`) are joined with `--host-label-separator`
(default `-`) and prefixed with `--host-name-prefix` (default none). Characters other than letters, digits,
`.`, `_` and `-` become `_`. The first label is required: alerts without it go to `--icinga-hostname`.
Later labels that an alert does not carry are left out, unless the mapping file gives them a default.

With polling, the labels of each Alertmanager in `alertmanagers.yml` are set on its alerts. Webhook
alerts must carry them, e.g. as Prometheus `external_labels` or via `alert_relabel_configs`.

### Mapping label values

`--host-mapping-file` (e.g. `/etc/alertmanager-icinga-bridge/host-mapping.yml`) translates label values,
for example Rancher project IDs to project names
(see [`contrib/host-mapping.example.yaml`](contrib/host-mapping.example.yaml)):

```yaml
defaults:
  project: cluster          # optional: value for alerts without a "project" label
labels:
  project:
    xraydmz2/p-q2v7w: system   # only for cluster xraydmz2
    p-shared: shared           # for every cluster
```

A key can be scoped by the (original) values of the host labels that come before it, joined by `/`;
scoped keys take precedence over plain ones. The file is checked at startup and re-read on `SIGHUP`
(`systemctl reload`); if the new file is broken, the error is logged and the previous mapping stays.
Hosts that already exist are not renamed: alerts go to the new host name, and the old host is removed
by the garbage collection once its services are gone.

### Hosts created on demand

When an alert arrives for a host that does not exist yet, the bridge creates it with the
`--host-templates` (default `generic-host`), `--host-check-command` (default `dummy`, empty leaves it
to the templates), `--host-zone` and these custom variables:

| Variable | Value |
| -------- | ----- |
| `vars.<label>` (e.g. `vars.cluster`, `vars.project`) | label value after mapping (`system`) |
| `vars.<label>_source` (e.g. `vars.project_source`) | original value of a mapped label (`p-q2v7w`) |
| `vars.bridge_uuid` | the bridge's `--id` |
| `vars.bridge_created` | creation time (Unix seconds) |
| `--static-host-vars` | as given |

Use them in apply rules, host groups and notifications, e.g. `assign where host.vars.cluster == "xraydmz2"`.
Known hosts are cached for `--host-cache-ttl` (default 10m), so usually only the first alert for a host
costs an extra API call.

## Polling Alertmanagers

With `--alertmanagers-file`, the bridge polls the Alertmanagers' API (`GET /api/v2/alerts`, no credentials
needed) and reconciles Icinga with it
(see [`contrib/alertmanagers.example.yaml`](contrib/alertmanagers.example.yaml)):

```yaml
alertmanagers:
  - name: xraydmz2
    url: https://alertmanager.xraydmz2.prod.example.com
    labels:
      cluster: xraydmz2
```

Every `--poll-interval` (default 1m), for each Alertmanager (`--poll-concurrency` at a time):

* **Active alerts** (including silenced ones, but not inhibited ones, which Alertmanager does not notify
  about either) become services as described above. A result is only submitted if Icinga does not already
  show it, so a quiet poll costs one Alertmanager request and no Icinga writes; heartbeat alerts are always
  submitted.
* **The Alertmanager's `labels`** are set on every alert (overriding the alert's own values) and identify
  its services: those whose `vars.label_<name>` match. They are required.
* **Gone alerts**: a non-OK service of this Alertmanager whose alert was missing in `--poll-resolve-after`
  polls in a row (default 2) is set to OK. Nothing is resolved while the Alertmanager cannot be polled.
* **Silences** become fixed downtimes until the silence ends, with the author
  `alertmanager-icinga-bridge/<id>` and the silence in the comment; downtimes of expired or deleted
  silences are removed (`--no-poll-silences` turns this off).
* **Reachability**: the service `alertmanager` on the cluster's host (from the labels; on
  `alertmanager-bridge` named `alertmanager-<name>` if they do not form a host) is OK with the number of
  alerts as perfdata, CRITICAL (hard after 3 failed polls) if the Alertmanager cannot be polled, and
  UNKNOWN if the bridge stops polling.

Choose what to poll with `--poll-filter` (Alertmanager matchers such as `severity=~"warning|critical"`,
repeatable; per Alertmanager `filter`) and `--poll-receiver` (a regular expression for the receivers the
alerts are routed to; per Alertmanager `receiver`), as alerts with an unknown severity become UNKNOWN in
Icinga. The file is checked at startup and re-read on `SIGHUP`. Use `--poll-ca-file` for an internal CA.

## Webhook

Polling notices changes within `--poll-interval`; the webhook (`POST /webhook`) delivers Alertmanager's
notifications immediately. Both can be combined, polling then corrects what the webhook misses (an alert
silenced while firing never sends "resolved", notifications sent while the bridge is down are lost).

Every Alertmanager sends its notifications with a bearer token; `--bearer-token` can be repeated (or given
comma-separated in `ALERTMANAGER_ICINGA_BRIDGE_BEARER_TOKEN`), so each cluster can get its own token and be
revoked separately. Without tokens, the webhook accepts nothing. Listen on a reachable address
(`--listen-addr 0.0.0.0:8888`) and enable TLS (`--tls-cert-path`, `--tls-key-path`). `GET /healthz` returns
HTTP 200 while the listener runs.

Alertmanager configuration in each cluster:

```yaml
route:
  receiver: icinga
receivers:
- name: icinga
  webhook_configs:
  - url: https://icinga.example.com:8888/webhook
    send_resolved: true
    http_config:
      authorization:
        credentials_file: /etc/alertmanager/secrets/icinga-bridge-token
```

If one alert of a notification fails, the others are still processed and the bridge answers with
HTTP 500, so Alertmanager retries the notification.

## Alerts and services

Each alert becomes a service on its host, named `<alertname>_<hash of the labels>` (the labels in
`--alert-fingerprint-excludes`, default `severity`, are left out of the hash).

Alert fields:
* `generatorURL`: the service's `action_url`

Alert labels:
* `alertname`: the service's `display_name` (**required**; `--display-name-as-service-name` uses the
  service name instead)
* `severity`: the state; `warning` and `critical`, `normal` (OK), or values set with
  `--custom-severity-levels` (e.g. `info=0`); others become UNKNOWN (**required**)

Alert annotations:
* `description`: the plugin output (`--plugin-output-annotations`, default `description,message`: the
  first one the alert has) and the service's `notes` (`--notes-text-annotations`)
* `message`: the plugin output of alerts without a `description`
* `runbook_url`: the service's `notes_url` (`--notes-url-annotations`)

A plugin output or notes text ending in `on cluster .` gets the alert's `cluster` label inserted
(`on cluster xraydmz2.`): alert rules often reference a `cluster` label that is only added after the rule
was evaluated, as an external label or as a label of a polled Alertmanager.

### Plugin output

The plugin output is the first of the `--plugin-output-annotations` (default `description,message`) that
the alert has. With `--plugin-output-by-states`, the state name is appended to the annotation name first
(`message_ok`, `message_warning`, `message_critical`, `message_unknown`), falling back to the plain name.

### Custom variables

All alert labels and annotations are set as custom variables, prefixed with `label_` and `annotation_`.
Labels and annotations named `icinga_<type>_<name>` are also set as `<name>`, with the types `number` and
`string`; an annotation wins over a label of the same name.

| Alert      | Icinga      |
| ---------- | ----------- |
| Label: `foo: bar` | Custom Var: `label_foo = bar` |
| Annotation: `foo: bar` | Custom Var: `annotation_foo = bar` |
| Label: `icinga_string_foo: bar` | Custom Var: `foo = bar` |
| Label: `icinga_number_foo: 123` | Custom Var: `foo = 123` |

`--static-service-vars` adds variables to every service.

### Overrides per alert

| Alert label | Effect |
| ----------- | ------ |
| `icinga_use_host: MyHost` | Put the service on this existing host instead of the derived one (the API user needs permissions for it) |
| `icinga_use_host_check: "true"` | Together with `icinga_use_host`: set that host's state instead (firing: DOWN, resolved: UP) |
| `icinga_use_zone: MyZone` | Zone of the service (must exist) |
| `icinga_use_template: MyTemplate` | Additional template of the service (must exist) |

### Heartbeat alerts

An alert with a label `heartbeat` set to a [Go duration](https://pkg.go.dev/time#ParseDuration) (e.g.
`heartbeat: "24h"`) becomes an active dummy check with that interval plus 10%: every alert result resets it
to OK, and if none arrives in time it takes the alert's severity. Use it for `Watchdog` / dead man's switch
alerts. Garbage collection removes such services only while they are in a downtime.

## Cleanup

Every `--gc-interval` (default 15m):

* **Services** of the bridge (`vars.bridge_uuid` matches `--id`) are removed when they are OK and their
  last state change was more than `vars.keep_for` (from `--keep-for`, default 168h, stored in seconds)
  ago. The bridge's own services
  (`heartbeat`, `alertmanager`) are kept.
* **Hosts** created by the bridge are removed when no service of the bridge is left on them and they were
  created more than `--host-keep-for` (default 168h) ago; `--host-keep-for=0` keeps them.
  `alertmanager-bridge` is never removed.

## Self-monitoring

Every `--heartbeat-interval` (default 1m), the bridge submits OK to `alertmanager-bridge!heartbeat`
(`--icinga-hostname`, `--heartbeat-service`). The bridge creates the host and the service if they do not
exist; the service turns CRITICAL when no heartbeat arrived for three intervals. With polling, every
Alertmanager's `alertmanager` service shows whether it can be polled.

## Icinga configuration

[`contrib/icinga2-example.conf`](contrib/icinga2-example.conf) has templates, host groups and an API user
whose permissions are limited to the objects of the bridge (`host.vars.bridge_uuid`). The API user needs:

* `objects/query/*`, `objects/create/host`, `objects/delete/host`
* `objects/create/service`, `objects/modify/service`, `objects/delete/service`
* `actions/process-check-result`
* for polling with silences: `actions/schedule-downtime`, `actions/remove-downtime`

If you create `alertmanager-bridge` yourself, set `vars.bridge_uuid` on it as well, or the permissions
above do not cover it.

## Configuration

All flags can also be set with the environment variables shown. The bridge respects `HTTP_PROXY`,
`HTTPS_PROXY` and `NO_PROXY`.

```
Flags:
  -h, --help                       Show context-sensitive help.
      --id=STRING                  Instance ID ($ALERTMANAGER_ICINGA_BRIDGE_ID)
      --loglevel=info              Loglevel (debug, info, warn, error)
                                   ($ALERTMANAGER_ICINGA_BRIDGE_LOGLEVEL)
      --version                    Print version information and quit
      --icinga-url=ICINGA-URL,...
                                   Icinga API URL (can be repeated)
                                   ($ALERTMANAGER_ICINGA_BRIDGE_ICINGA_URL)
      --icinga-hostname="alertmanager-bridge"
                                   Icinga host of the bridge itself:
                                   heartbeat service and alerts without
                                   host labels (created if missing)
                                   ($ALERTMANAGER_ICINGA_BRIDGE_ICINGA_HOSTNAME)
      --disable-keep-alives        Disable HTTP keepalives
                                   ($ALERTMANAGER_ICINGA_BRIDGE_DISABLE_KEEPALIVES)
      --display-name-as-service-name
                                   Set the Icinga service display
                                   name to the generated service name
                                   ($ALERTMANAGER_ICINGA_BRIDGE_DISPLAY_NAME_AS_SERVICE_NAME)
      --icinga-insecure-tls        Skip Icinga TLS verification
                                   ($ALERTMANAGER_ICINGA_BRIDGE_ICINGA_INSECURE_TLS)
      --icinga-ca-file=STRING      Path of a custom CA certificate to
                                   use when connecting to the Icinga API
                                   ($ALERTMANAGER_ICINGA_BRIDGE_ICINGA_CA)
      --icinga-password=STRING     Icinga API password
                                   ($ALERTMANAGER_ICINGA_BRIDGE_ICINGA_PASSWORD)
      --icinga-user=STRING         Icinga API username
                                   ($ALERTMANAGER_ICINGA_BRIDGE_ICINGA_USERNAME)
      --custom-severity-levels=KEY=VALUE;...
                                   Add or override the default mapping
                                   of severity levels to service states
                                   (severity_level=service_state)
                                   ($ALERTMANAGER_ICINGA_BRIDGE_ALERTMANAGER_CUSTOM_SEVERITY_LEVELS)
      --gc-interval=15m            Interval to check for and
                                   remove created services
                                   ($ALERTMANAGER_ICINGA_BRIDGE_GC_INTERVAL)
      --heartbeat-interval=1m      Interval for the bridge
                                   self-monitoring service heartbeat
                                   ($ALERTMANAGER_ICINGA_BRIDGE_HEARTBEAT_INTERVAL)
      --heartbeat-service="heartbeat"
                                   The name for the bridge
                                   self-monitoring service
                                   ($ALERTMANAGER_ICINGA_BRIDGE_HEARTBEAT_SERVICE)
      --listen-addr="127.0.0.1:8888"
                                   Listening address for the
                                   incoming Alertmanager requests
                                   ($ALERTMANAGER_ICINGA_BRIDGE_LISTEN_ADDR)
      --bearer-token=BEARER-TOKEN,...
                                   Bearer token for incoming requests (can
                                   be repeated, e.g. one per Alertmanager;
                                   required unless only polling is used)
                                   ($ALERTMANAGER_ICINGA_BRIDGE_BEARER_TOKEN)
      --tls-cert-path=STRING       Path of a certificate file for
                                   TLS-enabled webhook endpoint (full chain)
                                   ($ALERTMANAGER_ICINGA_BRIDGE_TLS_CERT)
      --tls-key-path=STRING        Path of a private key file for
                                   TLS-enabled webhook endpoint
                                   ($ALERTMANAGER_ICINGA_BRIDGE_TLS_KEY)
      --check-command="dummy"      Specify Icinga check command
                                   during service creation
                                   ($ALERTMANAGER_ICINGA_BRIDGE_SERVICE_CHECKS_COMMAND)
      --active-checks              Create Icinga services as active checks
                                   ($ALERTMANAGER_ICINGA_BRIDGE_SERVICE_CHECKS_ACTIVE)
      --plugin-output-by-states    Enable dynamic selection of plugin
                                   output annotation based on service state
                                   ($ALERTMANAGER_ICINGA_BRIDGE_PLUGINOUTPUT_BY_STATES)
      --max-check-attempts=1       The maximum number of checks which are
                                   executed before changing to a hard state
                                   ($ALERTMANAGER_ICINGA_BRIDGE_SERVICE_MAX_CHECK_ATTEMPTS)
      --templates=generic-service,...
                                   Create Icinga services with the
                                   given template (can be repeated)
                                   ($ALERTMANAGER_ICINGA_BRIDGE_SERVICE_TEMPLATE)
      --icinga-host-object-labels=icinga_use_host,...
                                   List of Labels depicting the Icinga
                                   Host object to associate the alert
                                   service with (uses --icinga-hostname
                                   if no label is found on alerts)
                                   ($ALERTMANAGER_BRIDGE_ICINGA_HOST_OBJECT_LABELS)
      --icinga-host-check-labels=icinga_use_host_check,...
                                   List of Labels to be used to associate
                                   the incoming alert with the Icinga
                                   Host object instead of a Service object
                                   ($ALERTMANAGER_BRIDGE_ICINGA_HOST_CHECK_LABELS)
      --icinga-host-zone-labels=icinga_use_zone,...
                                   List of Labels to be used to set the zone
                                   attribute of the Icinga service object
                                   ($ALERTMANAGER_BRIDGE_ICINGA_HOST_ZONE_LABELS)
      --icinga-host-template-labels=icinga_use_template,...
                                   List of Labels to be used to set the
                                   included template for the Icinga service
                                   object (in addition to --templates)
                                   ($ALERTMANAGER_BRIDGE_ICINGA_HOST_TEMPLATE_LABELS)
      --plugin-output-annotations=description,message,...
                                   Annotations for the plugin
                                   output of the Icinga service;
                                   the first one the alert has is used
                                   ($ALERTMANAGER_ICINGA_BRIDGE_PLUGINOUTPUT_ANNOTATIONS)
      --notes-text-annotations=description,...
                                   List of Annotations to be used to set
                                   the notes text for the Icinga service
                                   ($ALERTMANAGER_BRIDGE_NOTES_TEXT_ANNOTATIONS)
      --notes-url-annotations=runbook_url,...
                                   List of Annotations to be used to set
                                   the notes URL for the Icinga service
                                   ($ALERTMANAGER_BRIDGE_NOTES_URL_ANNOTATIONS)
      --alert-fingerprint-excludes=severity,...
                                   Alert labels to exclude from calculating
                                   the fingerprint (can be repeated)
                                   ($ALERTMANAGER_ICINGA_BRIDGE_ALERT_FINGERPRINT_EXCLUDES)
      --checks-interval=12h        Interval (in seconds) to be used for
                                   Icinga check_interval and retry_interval
                                   ($ALERTMANAGER_ICINGA_BRIDGE_SERVICE_CHECKS_INTERVAL)
      --keep-for=168h              How long to keep created alerts
                                   around after they have been resolved
                                   ($ALERTMANAGER_ICINGA_BRIDGE_KEEP_FOR)
      --static-service-vars=KEY=VALUE;...
                                   Custom variable to be set for created Icinga
                                   services (variable=value, can be repeated)
                                   ($ALERTMANAGER_ICINGA_BRIDGE_STATIC_SERVICE_VAR)
      --host-labels=cluster,project,...
                                   Alert labels whose values form the Icinga
                                   host name (hosts are created on demand)
                                   ($ALERTMANAGER_ICINGA_BRIDGE_HOST_LABELS)
      --host-label-separator="-"
                                   Separator between the label
                                   values in derived host names
                                   ($ALERTMANAGER_ICINGA_BRIDGE_HOST_LABEL_SEPARATOR)
      --host-name-prefix=STRING    Prefix for derived host names
                                   ($ALERTMANAGER_ICINGA_BRIDGE_HOST_NAME_PREFIX)
      --host-mapping-file=STRING
                                   YAML/JSON file that maps label values (e.g.
                                   project IDs to names) for derived
                                   host names; reloaded on SIGHUP
                                   ($ALERTMANAGER_ICINGA_BRIDGE_HOST_MAPPING_FILE)
      --host-templates=generic-host,...
                                   Templates for created Icinga
                                   hosts (can be repeated)
                                   ($ALERTMANAGER_ICINGA_BRIDGE_HOST_TEMPLATE)
      --host-check-command="dummy"
                                   Check command for created Icinga hosts
                                   (empty: leave it to the templates)
                                   ($ALERTMANAGER_ICINGA_BRIDGE_HOST_CHECK_COMMAND)
      --host-zone=STRING           Zone for created Icinga hosts
                                   ($ALERTMANAGER_ICINGA_BRIDGE_HOST_ZONE)
      --static-host-vars=KEY=VALUE;...
                                   Custom variable to be set for created Icinga
                                   hosts (variable=value, can be repeated)
                                   ($ALERTMANAGER_ICINGA_BRIDGE_STATIC_HOST_VAR)
      --host-cache-ttl=10m         How long a host is assumed to
                                   exist before it is looked up again
                                   ($ALERTMANAGER_ICINGA_BRIDGE_HOST_CACHE_TTL)
      --host-keep-for=168h         Remove created hosts that have no
                                   bridge services left and are older
                                   than this (0 disables host removal)
                                   ($ALERTMANAGER_ICINGA_BRIDGE_HOST_KEEP_FOR)
      --alertmanagers-file=STRING
                                   YAML file listing the Alertmanagers
                                   whose active alerts are polled;
                                   reloaded on SIGHUP (empty disables polling)
                                   ($ALERTMANAGER_ICINGA_BRIDGE_ALERTMANAGERS_FILE)
      --poll-interval=1m           Interval between polls of the Alertmanagers
                                   ($ALERTMANAGER_ICINGA_BRIDGE_POLL_INTERVAL)
      --poll-filter=POLL-FILTER    Alertmanager matcher the polled alerts must
                                   match, e.g. severity=~"warning|critical"
                                   (can be repeated)
                                   ($ALERTMANAGER_ICINGA_BRIDGE_POLL_FILTER)
      --poll-receiver=STRING       Only poll alerts routed to receivers
                                   matching this regular expression
                                   ($ALERTMANAGER_ICINGA_BRIDGE_POLL_RECEIVER)
      --poll-resolve-after=2       Resolve a service once its alert was
                                   missing in this many polls in a row
                                   ($ALERTMANAGER_ICINGA_BRIDGE_POLL_RESOLVE_AFTER)
      --[no-]poll-silences         Mirror silences of polled
                                   alerts as Icinga downtimes
                                   ($ALERTMANAGER_ICINGA_BRIDGE_POLL_SILENCES)
      --poll-concurrency=8         Number of Alertmanagers
                                   polled at the same time
                                   ($ALERTMANAGER_ICINGA_BRIDGE_POLL_CONCURRENCY)
      --poll-ca-file=STRING        Path of a CA certificate for the
                                   Alertmanager URLs (default: system CAs)
                                   ($ALERTMANAGER_ICINGA_BRIDGE_POLL_CA)
      --poll-insecure-tls          Skip TLS verification of
                                   the Alertmanager URLs
                                   ($ALERTMANAGER_ICINGA_BRIDGE_POLL_INSECURE_TLS)
```
