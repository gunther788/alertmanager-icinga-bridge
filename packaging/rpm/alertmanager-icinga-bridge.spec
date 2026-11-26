# Build with `make rpm`, which creates the source tarball (including the
# vendored Go modules, so the build needs no network access) and passes
# pkg_version, pkg_release and gobin. Go >= the version in go.mod is required;
# it is not a BuildRequires because EL9 ships an older one.

%global debug_package %{nil}
%global service alertmanager-icinga-bridge

%{!?pkg_version: %global pkg_version 0.0.0}
%{!?pkg_release: %global pkg_release 1}
%{!?pkg_commit: %global pkg_commit unknown}
%{!?gobin: %global gobin go}
%{!?_pkgdocdir: %global _pkgdocdir %{_docdir}/%{name}-%{version}}

%bcond_without check

Name:           alertmanager-icinga-bridge
Version:        %{pkg_version}
Release:        %{pkg_release}%{?dist}
Summary:        Bridge from Prometheus Alertmanager webhooks to Icinga 2

# The bridge is BSD-3-Clause, internal/icinga2 is Apache-2.0;
# vendored: kong (MIT), go.yaml.in/yaml (Apache-2.0 AND MIT)
License:        BSD-3-Clause AND Apache-2.0 AND MIT
URL:            https://github.com/NETWAYS/alertmanager-icinga-bridge
Source0:        %{name}-%{version}.tar.gz

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
export CGO_ENABLED=0
export GOFLAGS="-mod=vendor -trimpath"
export GOTOOLCHAIN=local
%{gobin} build \
    -ldflags "-s -w -B gobuildid -X main.version=%{version} -X main.commit=%{pkg_commit}" \
    -o %{name} .

%if %{with check}
%check
export CGO_ENABLED=0
export GOFLAGS="-mod=vendor"
export GOTOOLCHAIN=local
%{gobin} test ./...
%endif

%install
install -D -m 0755 %{name} %{buildroot}%{_bindir}/%{name}
install -D -m 0644 packaging/systemd/%{service}.service %{buildroot}%{_unitdir}/%{service}.service
install -D -m 0644 packaging/systemd/%{service}.sysusers %{buildroot}%{_sysusersdir}/%{name}.conf
install -D -m 0600 packaging/systemd/%{service}.sysconfig %{buildroot}%{_sysconfdir}/sysconfig/%{service}
install -d -m 0750 %{buildroot}%{_sysconfdir}/%{name}
cat > %{buildroot}%{_sysconfdir}/%{name}/host-mapping.yaml <<'MAPPING'
# Host mapping, used when ALERTMANAGER_ICINGA_BRIDGE_HOST_MAPPING_FILE points here.
# Reload with `systemctl reload alertmanager-icinga-bridge`.
# See %{_pkgdocdir}/host-mapping.example.yaml for the format.
defaults: {}
labels: {}
MAPPING
chmod 0644 %{buildroot}%{_sysconfdir}/%{name}/host-mapping.yaml

%pre
getent group %{name} >/dev/null || groupadd -r %{name}
getent passwd %{name} >/dev/null || \
    useradd -r -g %{name} -d / -s /sbin/nologin -c "Alertmanager Icinga Bridge" %{name}
exit 0

%post
%systemd_post %{service}.service

%preun
%systemd_preun %{service}.service

%postun
%systemd_postun_with_restart %{service}.service

%files
%license LICENSE
%doc README.md contrib/icinga2-example.conf contrib/icinga2-multicluster.conf contrib/host-mapping.example.yaml
%{_bindir}/%{name}
%{_unitdir}/%{service}.service
%{_sysusersdir}/%{name}.conf
%config(noreplace) %{_sysconfdir}/sysconfig/%{service}
%dir %attr(0750,root,%{name}) %{_sysconfdir}/%{name}
%config(noreplace) %{_sysconfdir}/%{name}/host-mapping.yaml

%changelog
* Thu Nov 26 2026 alertmanager-icinga-bridge maintainers - 0.0.0-1
- Package with systemd unit, sysconfig file and host mapping file
