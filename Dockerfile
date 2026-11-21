# syntax=docker/dockerfile:1

# velruna is a static Go binary: the runtime image needs the file and nothing
# else. The build stage pins the Go version to the go.mod directive so an image
# and a local build cannot drift.
FROM golang:1.26 AS build
WORKDIR /src

# Dependency layer first: source edits then rebuild without re-downloading.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Version and commit are stamped exactly like `make build` does: the binary
# reports them in `velruna status`, so an image can be traced to a tag.
ARG VERSION=0.0.0-dev
ARG COMMIT=unknown
RUN CGO_ENABLED=0 go build -trimpath \
        -ldflags "-s -w \
            -X github.com/NemoNobody/velruna/internal/version.Version=${VERSION} \
            -X github.com/NemoNobody/velruna/internal/version.Commit=${COMMIT}" \
        -o /out/velruna .

# distroless/static carries CA certificates (HTTPS to OpenSearch) and nothing
# else: no shell, no package manager, so code execution has nothing to run.
# nonroot = uid 65532; mount a config the operator owns and keeps at 0600.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/velruna /velruna

# A container has no session bus, so the OS keyring is unreachable: the
# password comes from OPENSEARCH_PASSWORD or the mounted config file.
ENV VELRUNA_CONF=/etc/velruna/velruna.conf

ENTRYPOINT ["/velruna"]
