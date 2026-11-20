# Development entry points. The defaults match .github/workflows/ci.yml so a
# local `make ci` reproduces what the CI gate enforces.

# Working tree version carries the next-release number with a -dev suffix; the
# real (suffix-less) number is set only when cutting a tag:
# make release VERSION=v0.8.0. Tag builds stamp the tag name via release.yml.
VERSION ?= 0.8.5
# Commit stamps the exact tree into the binary (memento_status reports it).
# `?=` + shell fallback: local builds get the live HEAD; CI can pass it explicitly.
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null)
LDFLAGS := -s -w -X github.com/NemoNobody/memento/internal/version.Version=$(VERSION) -X github.com/NemoNobody/memento/internal/version.Commit=$(COMMIT)
BIN     ?= memento

CONTAINER ?= $(shell if command -v docker >/dev/null 2>&1; then echo "docker compose"; elif command -v podman-compose >/dev/null 2>&1; then echo "podman-compose"; else echo "podman compose"; fi)
OSFILE := docker/opensearch.yml

.PHONY: build test lint fmt vet ci install clean release dev-opensearch dev-opensearch-down dev-opensearch-logs dev-opensearch-dashboards dev-opensearch-cluster dev-opensearch-secure

# Compose profiles in docker/opensearch.yml. `down` must name them all or
# profiled services survive it.
OS_PROFILES := --profile single --profile dashboards --profile cluster --profile secure

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) .

test:
	go test ./...

# Keyring integration: a disposable session bus + gnome-keyring with a
# throwaway HOME, so the developer's real wallet is never touched. The tests
# are opt-in (MEMENTO_KEYRING_TEST=1) because a plain `go test` must not write
# to a live keychain; CI runs this target on Linux and the equivalent against
# the macOS Keychain (internal/keyring).
test-keyring:
	@command -v dbus-run-session >/dev/null 2>&1 || { echo "test-keyring needs dbus-run-session (dbus) and gnome-keyring-daemon"; exit 1; }
	@command -v gnome-keyring-daemon >/dev/null 2>&1 || { echo "test-keyring needs gnome-keyring-daemon (package: gnome-keyring)"; exit 1; }
	dbus-run-session -- bash -c 'h=$$(mktemp -d); export HOME=$$h; eval "$$(echo -n memento-ci | gnome-keyring-daemon --unlock --replace --components=secrets)" >/dev/null 2>&1; MEMENTO_KEYRING_TEST=1 go test ./internal/keyringnative/ -v; rc=$$?; rm -rf "$$h"; exit $$rc'

vet:
	go vet ./...

fmt:
	gofmt -w .

lint:
	golangci-lint run ./...

ci: fmt-check build vet test lint

fmt-check:
	test -z "$$(gofmt -l .)"

# Install the release binary outside the work tree, which is the layout Zed
# and other long-lived clients should point at: a running server must not
# depend on a binary that `go build` overwrites mid-development.
install:
	mkdir -p $(HOME)/.local/bin
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(HOME)/.local/bin/$(BIN) .

# Dev/single-machine OpenSearch: single node, localhost-only,
# security plugin off (the dev rule), named data volume, snapshots on the host.
dev-opensearch:
	$(CONTAINER) -f $(OSFILE) --profile single up -d

dev-opensearch-down:
	$(CONTAINER) -f $(OSFILE) $(OS_PROFILES) down

dev-opensearch-logs:
	$(CONTAINER) -f $(OSFILE) logs --tail=100

# Optional OpenSearch Dashboards on top of the dev node (compose profile
# "dashboards"): make dev-opensearch first, then this. UI on
# http://127.0.0.1:5601 (MEMENTO_DASH_PORT to move it). The security plugin
# is off on the dev node, so the UI opens without login. The target waits
# for the UI to answer and then pushes the ready-made bundle (index
# patterns, visualizations, overview dashboard) into it automatically —
# re-runnable (stable object ids). For any other (secured) cluster use
# scripts/push-dashboards.sh by hand.
dev-opensearch-dashboards:
	$(CONTAINER) -f $(OSFILE) --profile single --profile dashboards up -d
	@echo "waiting for Dashboards on http://127.0.0.1:$${MEMENTO_DASH_PORT:-5601} ..."
	@ok=""; for i in $$(seq 1 60); do \
	  if curl -sf -o /dev/null --max-time 3 "http://127.0.0.1:$${MEMENTO_DASH_PORT:-5601}/api/status"; then ok=1; break; fi; \
	  sleep 3; \
	done; \
	if [ -z "$$ok" ]; then \
	  echo "dashboards did not answer within ~3 minutes"; exit 1; \
	fi; \
	for i in $$(seq 1 10); do \
	  if MEMENTO_OS_URL="http://127.0.0.1:$${MEMENTO_OS_PORT:-9200}" scripts/push-dashboards.sh; then ok=1; break; fi; \
	  ok=""; echo "bundle push failed (dashboards may still be initializing), retrying in 5s..."; sleep 5; \
	done; \
	if [ -z "$$ok" ]; then \
	  echo "bundle push still failing; manual retry: MEMENTO_OS_URL=http://127.0.0.1:$${MEMENTO_OS_PORT:-9200} scripts/push-dashboards.sh"; exit 1; \
	fi

# 3-node cluster (topology test): memento stamps replicas automatically
# (index_replicas=-1: 0 on a lone node, 1 on a real cluster). Endpoints:
# 127.0.0.1:9400/9401/9402. Point a config at it to smoke-test memento
# against real shard allocation.
dev-opensearch-cluster:
	$(CONTAINER) -f $(OSFILE) --profile cluster up -d

# single node WITH security (auth/https smoke): the image installs the
# demo security config using the password passed on the make command
# line — e.g. make dev-opensearch-secure MEMENTO_SECURE_DEV_PASSWORD=...
# then connect with opensearch_url=https://127.0.0.1:9443,
# opensearch_user=admin and insecure_skip_verify=true (demo certs).
dev-opensearch-secure:
	@test -n "$(MEMENTO_SECURE_DEV_PASSWORD)" || { echo "usage: make dev-opensearch-secure MEMENTO_SECURE_DEV_PASSWORD=<pw>"; exit 1; }
	$(CONTAINER) -f $(OSFILE) --profile secure up -d

# obscura: one-time installer for the opt-in render fallback (fetch_render=
# obscura). Third-party binary, ~75MB, linux x86_64 glibc 2.35+; the sha256
# is pinned to the audited 0.2.6 stealth build. obscura and obscura-worker
# must stay in one directory (upstream layout); the default
# fetch_obscura_path is "obscura" resolved through PATH, so ~/.local/bin
# must be on PATH. Enable with fetch_render=obscura in the config.
OBSCURA_VERSION ?= 0.2.6
OBSCURA_SHA256 ?= ac8057374ec1b5cabb78ea2d08b2ed890d21d156a42b1ac23357fd049edc08c5
obscura:
	@set -e; tmp=$$(mktemp /tmp/obscura-stealth-XXXXXX.tar.gz); \
	trap 'rm -f "$$tmp"' EXIT; \
	mkdir -p $(HOME)/.local/bin; \
	curl -fsSL --max-time 600 -o "$$tmp" \
		https://github.com/h4ckf0r0day/obscura/releases/download/v$(OBSCURA_VERSION)/obscura-x86_64-linux-stealth.tar.gz; \
	echo "$(OBSCURA_SHA256)  $$tmp" | sha256sum -c -; \
	tar -xzf "$$tmp" -C $(HOME)/.local/bin obscura obscura-worker; \
	chmod +x $(HOME)/.local/bin/obscura $(HOME)/.local/bin/obscura-worker
	@echo "obscura $(OBSCURA_VERSION) installed into $(HOME)/.local/bin; set fetch_render=obscura (env MEMENTO_FETCH_RENDER=obscura) to enable the render fallback"

clean:
	rm -f $(BIN)

# release: cut a versioned release: tag HEAD and push; .github/workflows/
# release.yml builds the binaries and publishes the GitHub release.
# Requires a clean tree; usage: make release VERSION=v0.1.0
release: test
	@printf '%s' "$(VERSION)" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$$' || { echo "usage: make release VERSION=v0.1.0"; exit 2; }
	git diff --exit-code
	git diff --cached --exit-code
	git tag -a $(VERSION) -m "release $(VERSION)"
	git push origin HEAD --follow-tags
