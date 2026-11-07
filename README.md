# memento

[![CI](https://github.com/NemoNobody/memento/actions/workflows/ci.yml/badge.svg)](https://github.com/NemoNobody/memento/actions/workflows/ci.yml)

Persistent, provenance-carrying memory for AI agents (and humans).

## What it is

`memento` stores **verified facts** about your projects — file locations,
endpoint and config values, behavior — in an OpenSearch index, so the same thing
is not re-verified over and over, by you or by an agent.

Each fact carries evidence, not verdicts:

- **provenance** (`source`): where it came from — `tool:read_file`,
  `experiment`, `user:confirmed`, `doc:*`;
- **observation time** (`observed_at`): when the value was actually seen;
- **history**: a changed value moves the old one to `history` instead of
  overwriting it; bounded per key by `history_limit` (default 20, oldest
  entries dropped first);
- **notes**: free-text qualifiers, e.g. "checked the dev config only";
- **secret guard**: writes scan for credential-like text (the index is
  plaintext) — `reject` by default, `ask` needs an explicit confirmation,
  `allow` opts out;
- **staleness**: reads compute `age_days` and `stale` from `observed_at` and a
  per-category TTL (`config`/`endpoint` 14 days, `location`/`behavior` 30 days,
  `context_map` and unknown categories never). `stale: true` is an instruction
  to re-verify against the source, not to discard the fact.

Two statuses: `verified` (the subject exists) and `obsolete` (the subject is
gone — file deleted, endpoint removed). The store holds the evidence; the agent
judges trust and staleness in context.

## Quick start

Build (Go 1.26.8+):

```
go build -o memento .
```

Or install directly (once the module is public and tagged):

```
go install github.com/NemoNobody/memento@latest
```

You also need a reachable OpenSearch (a single local node is enough) — install
it per the [official documentation](https://docs.opensearch.org/latest/install-and-configure/install-opensearch/index/);
see [docs/setup.md](docs/setup.md) for what `memento` expects.

Configure: copy `memento.conf.example` to `memento.conf` (or `~/.memento.conf`)
and fill in your OpenSearch connection.

```json
{
  "opensearch_url": "https://127.0.0.1:9200",
  "opensearch_user": "admin",
  "opensearch_password": "<your-password>",
  "insecure_skip_verify": true,
  "index_name": "memento",
  "history_limit": 20,
  "secret_guard": "reject",
  "allow_plaintext": false,
  "docs_index_name": "memento-docs",
  "docs_ttl_days": 90
}
```

The index is created automatically on first use. Env vars override the file:
`OPENSEARCH_URL`, `OPENSEARCH_USER`, `OPENSEARCH_PASSWORD`,
`OPENSEARCH_INSECURE_SKIP_VERIFY`, `MEMENTO_INDEX`, `MEMENTO_HISTORY_LIMIT`,
`MEMENTO_SECRET_GUARD`, `MEMENTO_ALLOW_PLAINTEXT`; `MEMENTO_CONF` points at an
explicit config file. Fetch
options (`memento fetch`): `MEMENTO_DOCS_INDEX`, `MEMENTO_DOCS_TTL_DAYS`,
`MEMENTO_FETCH_TIMEOUT`, `MEMENTO_FETCH_MAX_BYTES`, `MEMENTO_FETCH_HOSTS`,
`MEMENTO_FETCH_USER_AGENT`, `MEMENTO_FETCH_PROXY_URL`,
`MEMENTO_FETCH_PROXY_FALLBACK`, `MEMENTO_FETCH_RESPECT_ROBOTS`,
`MEMENTO_LOCAL_REPO_ROOTS`, `MEMENTO_REPO_SCAN_DEPTH`.

The password resolves from `OPENSEARCH_PASSWORD`, then the OS keyring (service
`memento`, account `host[:port]`), then the config file — only when
`allow_plaintext` is on (off by default). Store it in the keyring with
`memento creds set` (the password is never taken from argv); in containers and
headless setups export `OPENSEARCH_PASSWORD` instead. Check the connection:

```
memento status
```

Store and read back a fact:

```
memento add demo:api:port "API port is 8080" endpoint -p demo -s tool:read_file
memento get demo:api:port
memento search "api port"
```

Fetch a web page into the docs index and read it back section by section:

```
memento fetch https://example.com/docs
memento doc <hash-from-output> 0
memento docs "example"
```

## Commands

| Command | Description |
|---------|-------------|
| `memento add <key> <value> <category> [--project <p>] [--source <s>] [--notes <n>] [--allow-secret]` | Add or correct a fact (old value goes to history) |
| `memento get <key> [--history]` | Get a fact by key (history is off by default; `--history` prints the full chain) |
| `memento search <query> [--project <p>] [--category <c>] [--status <s>] [--limit N]` | Full-text search over values and notes (`--status any` disables the status filter; `--limit 0` = up to 1000) |
| `memento keys [prefix] [--match <s>] [--project <p>] [--status <s>]` | Find facts by key: prefix and/or substring (`*`, `?` wildcards in `--match`), across projects |
| `memento list <project> [--prefix <p>]` | List all facts for a project, optionally narrowed to a key prefix (trailing `*` allowed) |
| `memento prune <key> [--keep N] [--yes]` | Drop old history entries, keeping the newest N (destructive: without `--yes` only the plan is printed) |
| `memento obsolete <key> [reason] [--allow-secret]` | Mark a fact obsolete because its subject is gone (reason appended to notes) |
| `memento status [--conf <path>]` | Check connectivity and show the effective configuration (env overrides are listed by name, never values) |
| `memento creds set\|status\|rm [--url <u>]` | Manage the OpenSearch password in the OS keyring: `set` reads a hidden prompt (piped stdin for scripts), `status` shows only the source (`env\|keyring\|file\|none`), `rm` deletes the entry; the stored value is never printed |
| `memento fetch <url> [--refresh] [-p <p>] [--allow-secret]` | Fetch a page into the docs index and print its outline: cache-aware, section text is read on demand; github URLs that match a local clone print the file path |
| `memento doc <hash> [n\|--section N] [--full]` | Read a stored document: the outline (default), section `n`, or every section with `--full` |
| `memento docs <query> [--limit N] [-p <p>]` | BM25 search over stored documents (the extracted sections, never the raw page) |
| `memento mcp` | Run as an MCP server over stdio |

Key convention: the key names a **stable subject**, not its current value —
`demo:api:port`, not `demo:api:v1`. Re-using the same key with a new value is a
correction that keeps the old value in history; re-adding the same value updates
notes only.

## MCP server

`memento mcp` speaks MCP over stdio, so agents (VS Code, others) call
`memento_status`, `memento_search`, `memento_get`, `memento_add`, `memento_list`,
`memento_keys`, `memento_prune`, `memento_obsolete`, `memento_fetch_doc`,
`memento_get_doc` and `memento_search_docs` directly. Reads omit
history unless
`include_history=true` is passed, and carry `age_days`/`stale`. Registration and
troubleshooting: [docs/mcp.md](docs/mcp.md).

## Use with agents

An importable rule lives in [rules/memento.md](rules/memento.md): copy it — or
just its compact block — into `AGENTS.md`, `.github/copilot-instructions.md`,
`.cursor/rules/`, or your editor's always-on instructions. It tells the agent
when to consult and update memory, and how to keep it honest.

## Dashboards

`dashboards/memento-dashboard.ndjson` — a ready-made OpenSearch Dashboards
overview: totals, breakdowns by status/project/category/source, observation
activity, the oldest observations (re-verification queue), and facts with
notes. Import instructions and caveats: [docs/dashboard.md](docs/dashboard.md).

## Storage

Facts live in a single OpenSearch index (default `memento`). One node is enough
to start; a proper cluster works the same way. How to point `memento` at a
cluster: [docs/setup.md](docs/setup.md). Schema and design:
[docs/how-it-works.md](docs/how-it-works.md).

**Secrets do not belong in facts** (passwords, tokens, private keys) — a rule for
agents and humans, not a guarantee: the store is plaintext, and nothing filters
what gets written.

## Platform notes

The Linux keyring provider (native D-Bus Secret Service) is the daily driver
and is exercised live. The darwin/windows providers (system Keychain /
Credential Manager via zalando/go-keyring) are built in CI and unit-tested,
but not smoke-tested on real Apple/Windows hardware by the author. If you are
on those platforms and the keyring misbehaves, open an issue — reports from
those machines are welcome.

## License

[GPL-3.0](LICENSE)

