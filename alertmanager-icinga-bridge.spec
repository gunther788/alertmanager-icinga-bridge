# Built by the GitLab pipeline (template ops/packages/templates,
# .gitlab-ci-mock.yml): "mock --buildsrpm --sources=<checkout>" copies the
# whole checkout into the SOURCES directory, where this spec packs it into
# Source0 (see below), and the RPM is then rebuilt from the source RPM.
# By hand, the same works with:
#
#   rpmbuild -ba --define "_sourcedir $PWD" alertmanager-icinga-bridge.spec
#
# Go modules (and, if the distribution's Go is older than go.mod requires,
# the Go toolchain) are downloaded during the build, so it needs network
# access (mock --enable-network) unless the tarball contains a vendor
# directory. GOPROXY and GOSUMDB can point to a private proxy.
#
# Bump Version (or Release) for every change that should reach the
# repository; the pipeline builds whatever the spec says.
#
# Optional defines: pkg_commit (shown by --version, default: the commit of
# the checkout); "--without check" skips the tests.

%global debug_package %{nil}
%global service alertmanager-icinga-bridge

%{!?_pkgdocdir: %global _pkgdocdir %{_docdir}/%{name}-%{version}}

%bcond_without check

# Distribution Go packages set GOTOOLCHAIN=local (and may set GOPROXY=direct)
# in their go.env; settings from the build environment win over these defaults.
%global go_env \
export CGO_ENABLED=0 \
export GOTOOLCHAIN="${GOTOOLCHAIN:-auto}" \
export GOPROXY="${GOPROXY:-https://proxy.golang.org,direct}" \
export GOSUMDB="${GOSUMDB:-sum.golang.org}" \
if [ -d vendor ]; then export GOFLAGS="-mod=vendor -trimpath"; else export GOFLAGS="-mod=readonly -trimpath"; fi

Name:           alertmanager-icinga-bridge
Version:        0.4.0
Release:        1%{?dist}
Summary:        Bridge from Prometheus Alertmanager webhooks to Icinga 2

# The bridge is BSD-3-Clause, internal/icinga2 is Apache-2.0;
# linked in: kong (MIT), go.yaml.in/yaml (Apache-2.0 AND MIT)
License:        BSD-3-Clause AND Apache-2.0 AND MIT
URL:            https://github.com/NETWAYS/alertmanager-icinga-bridge
Source0:        %{name}-%{version}.tar.gz

# When the SOURCES directory is a copy of the checkout (it contains go.mod),
# pack it into Source0, with the commit from .git in a file COMMIT. When the
# source RPM is rebuilt, SOURCES holds only the tarball and this does nothing.
%(cd "%{_sourcedir}" 2>/dev/null && test -f go.mod && test -f %{name}.spec && {
    head=$(cat .git/HEAD 2>/dev/null)
    case "$head" in ("ref: "*) head=$(cat ".git/${head#ref: }" 2>/dev/null) ;; esac
    echo "${head:-unknown}" > COMMIT
    tmp=$(mktemp)
    tar -czf "$tmp" --exclude=./.git --exclude='./%{name}-*.tar.*' --exclude='./*.rpm' \
        --exclude=./rpmbuild --exclude=./.go \
        --transform 's,^\.,%{name}-%{version},' . &&
        mv "$tmp" %{name}-%{version}.tar.gz
    rm -f "$tmp"
} >/dev/null 2>&1)

# Shell expression, evaluated during the build
%{!?pkg_commit: %global pkg_commit $(cat COMMIT 2>/dev/null || echo unknown)}

# Any Go that can switch toolchains (>= 1.21), see above
BuildRequires:  golang >= 1.21
BuildRequires:  systemd-rpm-macros
Requires(pre):  shadow-utils
%{?systemd_requires}

%description
Receives alerts from the generic webhook receiver of one or many Prometheus
Alertmanagers and submits them to Icinga 2 as passive check results. Hosts
can be derived from alert labels (e.g. cluster and project) and are created
in Icinga on demand.

%prep
%autosetup -p1

%build
%{go_env}
go build \
    -ldflags "-s -w -B gobuildid -X main.version=%{version} -X main.commit=%{pkg_commit}" \
    -o %{name} .

%if %{with check}
%check
%{go_env}
go test ./...
%endif

%install
install -D -m 0755 %{name} %{buildroot}%{_bindir}/%{name}
install -D -m 0644 packaging/systemd/%{service}.service %{buildroot}%{_unitdir}/%{service}.service
install -D -m 0644 packaging/systemd/%{service}.sysusers %{buildroot}%{_sysusersdir}/%{name}.conf
install -d -m 0750 %{buildroot}%{_sysconfdir}/%{name}
install -m 0600 packaging/systemd/%{service}.environment %{buildroot}%{_sysconfdir}/%{name}/environment
cat > %{buildroot}%{_sysconfdir}/%{name}/host-mapping.yml <<'MAPPING'
# Host mapping, used when ALERTMANAGER_ICINGA_BRIDGE_HOST_MAPPING_FILE points here.
# Reload with `systemctl reload alertmanager-icinga-bridge`.
# See %{_pkgdocdir}/host-mapping.example.yaml for the format.
defaults: {}
labels: {}
MAPPING
chmod 0644 %{buildroot}%{_sysconfdir}/%{name}/host-mapping.yml
cat > %{buildroot}%{_sysconfdir}/%{name}/alertmanagers.yml <<'ALERTMANAGERS'
# Alertmanagers to poll, used when ALERTMANAGER_ICINGA_BRIDGE_ALERTMANAGERS_FILE points here.
# Reload with `systemctl reload alertmanager-icinga-bridge`.
# See %{_pkgdocdir}/alertmanagers.example.yaml for the format.
alertmanagers: []
ALERTMANAGERS
chmod 0644 %{buildroot}%{_sysconfdir}/%{name}/alertmanagers.yml

%pre
getent group %{name} >/dev/null || groupadd -r %{name}
getent passwd %{name} >/dev/null || \
    useradd -r -g %{name} -d / -s /sbin/nologin -c "Alertmanager Icinga Bridge" %{name}
exit 0

%post
%systemd_post %{service}.service

%posttrans
# Up to 0.2.0 the settings were /etc/sysconfig/%{service} and *.yaml files.
# Edited ones are left as .rpmsave by the upgrade: move them to the new names
# and keep the new defaults as .rpmnew. The service was already restarted by
# the old package with the defaults, so restart it again if it runs.
migrated=
migrate() {
    [ -f "$1" ] || return 0
    [ -e "$2" ] && cp -p "$2" "$2.rpmnew"
    sed -e 's#/host-mapping\.yaml#/host-mapping.yml#' -e 's#/alertmanagers\.yaml#/alertmanagers.yml#' \
        "$1" > "$2.tmp" || { rm -f "$2.tmp"; return 0; }
    chmod --reference="$2.rpmnew" "$2.tmp" 2>/dev/null
    mv -f "$2.tmp" "$2" && rm -f "$1" && migrated=1
    echo "warning: moved $1 to $2" >&2
}
migrate %{_sysconfdir}/sysconfig/%{service}.rpmsave %{_sysconfdir}/%{name}/environment
migrate %{_sysconfdir}/%{name}/host-mapping.yaml.rpmsave %{_sysconfdir}/%{name}/host-mapping.yml
migrate %{_sysconfdir}/%{name}/alertmanagers.yaml.rpmsave %{_sysconfdir}/%{name}/alertmanagers.yml
if [ -n "$migrated" ] && [ -d /run/systemd/system ]; then
    systemctl try-restart %{service}.service >/dev/null 2>&1 || :
fi
exit 0

%preun
%systemd_preun %{service}.service

%postun
%systemd_postun_with_restart %{service}.service

%files
%license LICENSE
%doc README.md contrib/icinga2-example.conf contrib/host-mapping.example.yaml contrib/alertmanagers.example.yaml
%{_bindir}/%{name}
%{_unitdir}/%{service}.service
%{_sysusersdir}/%{name}.conf
%config(noreplace) %{_sysconfdir}/%{name}/environment
%dir %attr(0750,root,%{name}) %{_sysconfdir}/%{name}
%config(noreplace) %{_sysconfdir}/%{name}/host-mapping.yml
%config(noreplace) %{_sysconfdir}/%{name}/alertmanagers.yml

%changelog
* Fri Nov 27 2026 alertmanager-icinga-bridge maintainers - 0.4.0-1
- The description annotation is the plugin output (default
  --plugin-output-annotations=description,message)
- "on cluster ." at the end of the output and notes gets the alert's
  cluster label

* Fri Nov 27 2026 alertmanager-icinga-bridge maintainers - 0.3.2-1
- vars.keep_for of services is in seconds (was nanoseconds); older values
  are still understood
- Example Icinga configuration documents the service variables

* Fri Nov 27 2026 alertmanager-icinga-bridge maintainers - 0.3.1-1
- Send API queries with a body as POST with X-HTTP-Method-Override: GET
- Log when an Icinga endpoint answers with HTTP 5xx and the next one is tried

* Fri Nov 27 2026 alertmanager-icinga-bridge maintainers - 0.3.0-1
- The bridge host is called alertmanager-bridge by default and is created,
  with its heartbeat service, if missing
- --host-labels defaults to cluster,project
- The default environment file polls alertmanagers.yml and no longer
  enables a placeholder webhook token
- One example Icinga configuration for the multi-cluster setup

* Fri Nov 27 2026 alertmanager-icinga-bridge maintainers - 0.2.2-1
- Report Icinga API errors (e.g. HTTP 401) when looking up single hosts
  and services instead of "no objects found"

* Fri Nov 27 2026 alertmanager-icinga-bridge maintainers - 0.2.1-1
- Settings in /etc/alertmanager-icinga-bridge/environment (was
  /etc/sysconfig/alertmanager-icinga-bridge), host-mapping.yml and
  alertmanagers.yml (were *.yaml); existing files are carried over

* Fri Nov 27 2026 alertmanager-icinga-bridge maintainers - 0.2.0-1
- Poll Alertmanagers (--alertmanagers-file): reconcile services with the
  active alerts, mirror silences as downtimes, report reachability

* Thu Nov 26 2026 alertmanager-icinga-bridge maintainers - 0.1.0-1
- First package: hosts derived from alert labels, systemd unit,
  environment file and host mapping file
