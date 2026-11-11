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

## Not yet covered (known gaps)

- **secure profile** (`make dev-opensearch-secure`, https + demo certs) —
  infrastructure ready, live run pending.
- **keyringnative on darwin/windows** — cross-compiled binaries are smoke-tested
  only on linux/amd64; platform keyring paths need real machines.
- The OpenSearch **Dashboards bundle** targets the current OSD line; older OSD
  versions are out of scope by decision (roadmap 34).
