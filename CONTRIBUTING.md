# Contributing to memento

Thanks for your interest! memento is a small, focused tool: an MCP server for
verified agent memory on top of OpenSearch. Changes that keep that focus are
the most welcome.

## Ground rules

- **Fix the root cause, not the symptom.** Every "it works" claim needs an
  executed test or a live run behind it — see `docs/testing.md` for what is
  already verified and how to extend it.
- Scope discipline: bug fixes and focused improvements are easier to land than
  sweeping refactors. For anything larger, open an issue first to agree on the
  shape.
- The store is plaintext by design: never add code that puts secrets into
  OpenSearch, and keep the secret-guard behaviour intact.

## Development

```
go build -o memento .     # build
make ci                   # vet + go test ./... + golangci-lint (the CI gate)
make dev-opensearch       # throwaway single-node OpenSearch (docker/podman)
```

Point a throwaway config at the dev node, e.g.
`/tmp/memento-dev.conf`:

```
{"opensearch_url":"http://127.0.0.1:9200","opensearch_user":"admin","opensearch_password":"devtest","allow_plaintext":true}
```

Then `MEMENTO_CONF=/tmp/memento-dev.conf ./memento status`. The dev stack has
security off and binds to loopback only — never expose it.

Useful suites:

- `hack/cli-smoke.sh` — 23 live checks against one store (safe on any store you
  allow a few KB of throwaway keys in);
- `hack/version-matrix.sh` — the same smoke across the verified OpenSearch
  version matrix;
- `make dev-opensearch-cluster` — 3-node topology playground.

## Pull requests

- One logical unit per commit; commit messages in English, explaining *why*.
- `make ci` must pass; add or adjust tests when behaviour changes — tests here
  assert the HTTP contract against the fake cluster (`internal/client/contract_test.go`)
  and the client logic around it.
- User-visible changes: update `docs/` in the same PR and add a `CHANGELOG.md`
  entry under "Unreleased".
- Breaking changes to the stored schema or the MCP tool surface are acceptable
  pre-1.0 but must be called out in the PR and the changelog (with a migration
  note when one is needed).

License note: the project is GPL-3.0; contributions fall under the same license.
