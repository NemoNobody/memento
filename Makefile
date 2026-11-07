# Development entry points. The defaults match .github/workflows/ci.yml so a
# local `make ci` reproduces what the CI gate enforces.

VERSION ?= dev
LDFLAGS := -s -w -X github.com/NemoNobody/memento/internal/version.Version=$(VERSION)
BIN     ?= memento

.PHONY: build test lint fmt vet ci install clean release

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
