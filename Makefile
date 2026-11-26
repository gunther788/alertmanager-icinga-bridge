.PHONY: test coverage lint vet rpm rpm-sources
.DEFAULT_GOAL := rpm

CONTAINER_RUNTIME?=podman

COMMIT := $(shell git rev-parse HEAD)
VERSION?=latest
IMAGE_REPOSITORY?=ghcr.io/netways/alertmanager-icinga-bridge

build-image:
	$(CONTAINER_RUNTIME) build --pull \
        --build-arg BRIDGE_VERSION=$(VERSION) \
        -t $(IMAGE_REPOSITORY):$(VERSION) .
release-image:
	$(CONTAINER_RUNTIME) push \
        $(IMAGE_REPOSITORY):$(VERSION)
build:
	mkdir -p dist; \
	CGO_ENABLED=0 go build -o dist/alertmanager-icinga-bridge
release-snapshot:
	goreleaser release --snapshot --clean
release:
	mkdir -p dist;\
	CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)" -o dist/alertmanager-icinga-bridge
lint:
	go fmt $(go list ./... | grep -v /vendor/)
vet:
	go vet $(go list ./... | grep -v /vendor/)
test:
	go test -v -cover -race ./...
coverage:
	go test -v -cover -coverprofile=coverage.out ./... &&\
	go tool cover -html=coverage.out -o coverage.html
clean:
	rm -rf dist/*

# RPM: version from the latest v* tag (0.0.0 without one); commits since the
# tag go into the release, e.g. 1.2.3-4.gabc1234
NAME := alertmanager-icinga-bridge
GO ?= go
GIT_DESCRIBE := $(shell git describe --tags --long --match 'v[0-9]*' 2>/dev/null)
ifneq ($(GIT_DESCRIBE),)
RPM_VERSION ?= $(shell echo '$(GIT_DESCRIBE)' | sed -E 's/^v//; s/-[0-9]+-g[0-9a-f]+$$//; s/-/~/g')
GIT_AHEAD := $(shell echo '$(GIT_DESCRIBE)' | sed -E 's/.*-([0-9]+)-g[0-9a-f]+$$/\1/')
else
RPM_VERSION ?= 0.0.0
GIT_AHEAD := $(shell git rev-list --count HEAD 2>/dev/null || echo 0)
endif
SHORT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
ifeq ($(GIT_AHEAD),0)
RPM_RELEASE ?= 1
else
RPM_RELEASE ?= 0.$(GIT_AHEAD).g$(SHORT_COMMIT)
endif
RPM_TOPDIR := $(CURDIR)/dist/rpmbuild
RPM_SPEC := packaging/rpm/$(NAME).spec
RPM_SOURCE := $(NAME)-$(RPM_VERSION)
RPMBUILD_FLAGS ?=

# Source tarball of the working tree (tracked and untracked, not ignored files)
# with the Go modules vendored, so rpmbuild needs no network access
rpm-sources:
	rm -rf $(RPM_TOPDIR)
	mkdir -p $(RPM_TOPDIR)/SOURCES $(RPM_TOPDIR)/stage/$(RPM_SOURCE)
	git ls-files -z --cached --others --exclude-standard | \
		tar --null --files-from=- -cf - | tar -xf - -C $(RPM_TOPDIR)/stage/$(RPM_SOURCE)
	cd $(RPM_TOPDIR)/stage/$(RPM_SOURCE) && $(GO) mod vendor
	tar -C $(RPM_TOPDIR)/stage -czf $(RPM_TOPDIR)/SOURCES/$(RPM_SOURCE).tar.gz $(RPM_SOURCE)
	rm -rf $(RPM_TOPDIR)/stage

rpm: rpm-sources
	rpmbuild -ba $(RPMBUILD_FLAGS) \
		--define "_topdir $(RPM_TOPDIR)" \
		--define "pkg_version $(RPM_VERSION)" \
		--define "pkg_release $(RPM_RELEASE)" \
		--define "pkg_commit $(COMMIT)" \
		--define "gobin $(shell $(GO) env GOROOT)/bin/go" \
		$(RPM_SPEC)
	mkdir -p dist
	find $(RPM_TOPDIR)/RPMS $(RPM_TOPDIR)/SRPMS -name '*.rpm' -exec cp {} dist/ \;
	@ls -1 dist/*.rpm
