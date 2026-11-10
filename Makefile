# Development entry points. The defaults match .github/workflows/ci.yml so a
# local `make ci` reproduces what the CI gate enforces.

VERSION ?= dev
# Commit stamps the exact tree into the binary (memento_status reports it).
# `?=` + shell fallback: local builds get the live HEAD; CI can pass it explicitly.
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null)
LDFLAGS := -s -w -X github.com/NemoNobody/memento/internal/version.Version=$(VERSION) -X github.com/NemoNobody/memento/internal/version.Commit=$(COMMIT)
BIN     ?= memento

CONTAINER ?= $(shell if command -v docker >/dev/null 2>&1; then echo "docker compose"; elif command -v podman-compose >/dev/null 2>&1; then echo "podman-compose"; else echo "podman compose"; fi)
OSFILE := docker/opensearch.yml

.PHONY: build test lint fmt vet ci install clean release dev-opensearch dev-opensearch-down dev-opensearch-logs

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) .

test:
	go test ./...

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

# Dev/single-machine OpenSearch (roadmap 11): single node, localhost-only,
# security plugin off (the dev rule), named data volume, snapshots on the host.
dev-opensearch:
	$(CONTAINER) -f $(OSFILE) up -d

dev-opensearch-down:
	$(CONTAINER) -f $(OSFILE) down

dev-opensearch-logs:
	$(CONTAINER) -f $(OSFILE) logs --tail=100

clean:
	rm -f $(BIN)

# release: cut a versioned release: tag HEAD and push; .github/workflows/
# release.yml builds the binaries and publishes the GitHub release.
# Requires a clean tree; usage: make release VERSION=v0.1.0
release: test
	@case "$(VERSION)" in v[0-9]*) ;; *) echo "usage: make release VERSION=v0.1.0"; exit 2 ;; esac
	git diff --exit-code
	git tag -a $(VERSION) -m "release $(VERSION)"
	git push origin HEAD --follow-tags
