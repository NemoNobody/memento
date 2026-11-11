# Testing — what is actually verified

This project treats tests as the agent's self-control: claims of "works" are
backed by executed suites, and this page records what those suites cover and
what was run live against real clusters.

## Unit & integration (mock transport)

- `make ci` = `go vet` + `go test ./...` + `golangci-lint` — the same gate CI
  runs on every push (`.github/workflows/ci.yml`).
- ~340 test functions exercise the HTTP contract against a fake OpenSearch
  (`httptest`): which endpoints are called, with what parameters and bodies —
  index creation, composable journal templates, the ingest pipeline, scripted
  corrections with optimistic concurrency (`seq_no`/`primary_term`), the prune
  scripts, wildcard reads with `ignore_unavailable`, search bodies.
- The fake transport is deliberate: the contract, not OpenSearch itself, is
  where the historical bugs lived (read-after-write, read-only stores).

## Live CLI smoke — `hack/cli-smoke.sh`

23 checks against one live store (throwaway `smoke:<runid>:` keys): status,
add/correction/get, journal writes, history-info, list/keys/search, obsolete,
secret-guard rejection, prune dry-run + `--yes`, migrate-history, docs search
on a missing index. Exit 0 only when everything passes.

## Version matrix — `hack/version-matrix.sh` (2026-11-11)

Brings the compose dev node up per image tag (fresh volume each time, so the
full infra-creation path runs) and executes the smoke above against it.

| OpenSearch | Result |
|---|---|
| 2.19.7 (last 2.x) | PASS 23/23 |
| 3.0.0 | PASS 23/23 |
| 3.3.2 | PASS 23/23 |
| 3.8.0 (production) | PASS 23/23 |
| 3.9.0 | PASS 23/23 |

Run it yourself: `hack/version-matrix.sh` (sequential; needs docker/podman).
The images can also be pinned for everyday dev via `OPENSEARCH_IMAGE` in
`docker/opensearch.yml`.

## Topology — 3-node cluster (2026-11-11)

`make dev-opensearch-cluster` (three nodes on 127.0.0.1:9400–9402): CRUD +
prune against real shard allocation, node kill (primary failover, writes
continue while yellow), node return (back to green, data intact). Automatic
replica stamping (`index_replicas: -1`) verified: 0 replicas on a lone data
node, 1 on a multi-node cluster.

## Keyring — real providers (`make test-keyring`)

The secret store is the one dependency a mock cannot vouch for: the wire
protocol, the default-collection resolution and the "missing entry" sentinel
are provider behavior. `make test-keyring` starts a disposable session bus
(`dbus-run-session`) and a `gnome-keyring-daemon` with a throwaway `HOME`, then
runs `internal/keyringnative` against it — the developer's real wallet is never
touched. The tests also exist behind `MEMENTO_KEYRING_TEST=1`, which is what CI
turns on:

- Linux: `make test-keyring` in the `build-test` job (Secret Service over D-Bus).
- macOS: the `macos` job runs `MEMENTO_KEYRING_TEST=1 go test ./internal/keyring/`
  against the runner's login Keychain (the zalando provider: `security` CLI).
- Windows: build + platform-neutral tests only; the Credential Manager path is
  unverified (no runner coverage yet).

A plain `go test ./...` never writes to a live keychain: the integration tests
skip unless the flag is set.

## Not yet covered (known gaps)

- **secure profile** (`make dev-opensearch-secure`, https + demo certs) —
  infrastructure ready, live run pending.
- **keyringnative on darwin/windows** — darwin is covered by the `macos` CI job
  (Keychain); the Windows Credential Manager path is still unverified.
- The OpenSearch **Dashboards bundle** targets the current OSD line; older OSD
  versions are out of scope by decision: adapting saved objects to every old
  OSD line is a maintenance tax with no paying user.
