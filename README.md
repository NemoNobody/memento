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
- **history**: every accepted value is appended to an append-only journal
  index (`<index>-history-<period-date>`, monthly by default) instead of
  overwriting the fact; entries are never rewritten and the journal is not
  pruned by default (write-forever), so history shows how old it is;
- **notes**: free-text qualifiers, e.g. "checked the dev config only";
- **authorship**: every write is stamped with who recorded it (the store's
  `security` user when the cluster has one, else config/OS user — a
  self-reported audit label, not authentication) and which model/host/session
  produced it; the birth labels are write-once, corrections move `updated_by`;
- **summary**: a value over `summary_min_len` must ship a short table-of-
  contents rendering (kept under `summary_max_len`), so store listings stay
  skimmable without pulling full values into context;
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

Authorship is audit metadata, not access control: a write records the store's
security user when the cluster exposes one, otherwise the configured
(`author`) or OS user, plus `model`/`host`/`session` labels. These birth labels
are write-once; every later write moves `updated_by`. The journal carries the
same per-entry, so a shared store shows who saw what when. Labels are
self-reported — a client can set any `author`; the field supports review and
attribution, not trust.

## Quick start

Build (Go 1.26.8+):

```
go build -o memento .
```

Or install directly (once the module is public and tagged):

```
go install github.com/NemoNobody/memento@latest
```

You also need a reachable OpenSearch (a single local node is enough). Quickest:
`make dev-opensearch` runs one in a container (localhost-only, named volume —
[docs/setup.md](docs/setup.md) §2). System install per the
[official documentation](https://docs.opensearch.org/latest/install-and-configure/install-opensearch/index/);
see [docs/setup.md](docs/setup.md) for what `memento` expects.

Configure: copy `memento.conf.example` to `memento.conf` (or `~/.memento.conf`)
and fill in your OpenSearch connection.

```json
{
  "opensearch_url": "https://127.0.0.1:9200",
  "opensearch_user": "admin",
  "insecure_skip_verify": false,
  "index_name": "memento",
  "history_index_period": "month",
  "history_limit": 20,
  "summary_min_len": 2000,
  "summary_max_len": 400,
  "secret_guard": "reject",
  "allow_plaintext": false,
  "docs_index_name": "memento-docs",
  "docs_ttl_days": 90
}
```

Do not put `opensearch_password` in the file: store the password in the OS
keyring with `memento creds set`, or export `OPENSEARCH_PASSWORD` in
containers/headless setups (a plaintext file password is only read with
`allow_plaintext: true`, off by default). For a self-signed local dev node
set `insecure_skip_verify: true` — dev only; verification is on by default.

The index is created automatically on first use. Env vars override the file:
`OPENSEARCH_URL`, `OPENSEARCH_USER`, `OPENSEARCH_PASSWORD`,
`OPENSEARCH_INSECURE_SKIP_VERIFY`, `MEMENTO_INDEX`, `MEMENTO_HISTORY_PERIOD`,
`MEMENTO_HISTORY_LIMIT`, `MEMENTO_SUMMARY_MIN_LEN`, `MEMENTO_SUMMARY_MAX_LEN`,
`MEMENTO_SECRET_GUARD`, `MEMENTO_ALLOW_PLAINTEXT`,
`MEMENTO_AUTHOR`, `MEMENTO_MODEL`, `MEMENTO_HOST`, `MEMENTO_SESSION`;
`MEMENTO_CONF` points at an explicit config file. Fetch
options (`memento fetch`): `MEMENTO_DOCS_INDEX`, `MEMENTO_DOCS_TTL_DAYS`,
`MEMENTO_FETCH_TIMEOUT`, `MEMENTO_FETCH_MAX_BYTES`, `MEMENTO_FETCH_HOSTS`,
`MEMENTO_FETCH_USER_AGENT`, `MEMENTO_FETCH_PROXY_URL`,
`MEMENTO_FETCH_PROXY_FALLBACK`, `MEMENTO_FETCH_RESPECT_ROBOTS`,
`MEMENTO_FETCH_ALLOW_PRIVATE`, `MEMENTO_FETCH_RENDER`,
`MEMENTO_FETCH_OBSCURA_PATH`, `MEMENTO_FETCH_OBSCURA_STEALTH`,
`MEMENTO_LOCAL_REPO_ROOTS`,
`MEMENTO_REPO_SCAN_DEPTH`.

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
| `memento add <key> <value> <category> [--project <p>] [--source <s>] [--notes <n>] [--summary <s>] [--allow-secret]` | Add or correct a fact (prints `added`/`updated`/`touched (same value)`; the accepted value is appended to the journal; values over `summary_min_len` require `--summary`) |
| `memento get <key> [--history]` | Get a fact by key (history is off by default; `--history` prints the legacy in-doc array — journal-era history lives in `memento journal`) |
| `memento search <query> [--project <p>] [--category <c>] [--status <s>] [--limit N]` | Full-text search over values, notes and summaries (`--status any` disables the status filter; `--limit 0` = up to 1000) |
| `memento keys [prefix] [--match <s>] [--project <p>] [--status <s>]` | Find facts by key: prefix and/or substring (`*`, `?` wildcards in `--match`), across projects |
| `memento list <project> [--prefix <p>]` | List all facts for a project, optionally narrowed to a key prefix (trailing `*` allowed) |
| `memento journal <key> [--limit N]` | Read the journal: the append-only history of a key, newest first |
| `memento history-info <key>` | Compact journal overview: writes, rediscoveries, time range (no entry text) |
| `memento prune <key> [--keep N] [--yes]` | Drop old journal entries, keeping the newest N (manual admin tool: destructive, without `--yes` only the plan is printed) |
| `memento migrate-history [--project <p>] [--yes]` | Move pre-journal in-doc history arrays into the journal (one-time; plan first, `--yes` to write) |
| `memento obsolete <key> [reason] [--allow-secret]` | Mark a fact obsolete because its subject is gone (reason appended to notes) |
| `memento status [--conf <path>]` | Check connectivity and show the effective configuration (env overrides are listed by name, never values) |
| `memento creds set\|status\|rm [--url <u>]` | Manage the OpenSearch password in the OS keyring: `set` reads a hidden prompt (piped stdin for scripts), `status` shows only the source (`env\|keyring\|file\|none`), `rm` deletes the entry; the stored value is never printed |
| `memento fetch <url> [--refresh] [-p <p>] [--allow-secret]` | Fetch a page into the docs index and print its outline: cache-aware, section text is read on demand; github URLs that match a local clone print the file path |
| `memento doc <hash> [n\|--section N] [--full]` | Read a stored document: the outline (default), section `n`, or every section with `--full` |
| `memento docs <query> [--limit N] [-p <p>]` | BM25 search over stored documents (the extracted sections, never the raw page) |
| `memento mcp` | Run as an MCP server over stdio |

Key convention: the key names a **stable subject**, not its current value —
`demo:api:port`, not `demo:api:v1`. Re-using the same key with a new value is a
correction: the new value is journaled and the fact points at it; re-adding the
same value updates notes only and leaves the journal untouched.

## MCP server

`memento mcp` speaks MCP over stdio, so agents (VS Code, others) call
`memento_status`, `memento_search`, `memento_get`, `memento_add`,
`memento_add_batch`, `memento_patch`, `memento_list`,
`memento_keys`, `memento_journal`, `memento_history_info`, `memento_prune`,
`memento_obsolete`, `memento_fetch_doc`, `memento_get_doc` and
`memento_search_docs` directly. Reads omit
history unless
`include_history=true` is passed (that chain is the legacy pre-journal
in-doc array; journal history is read with `memento_journal` /
`memento_history_info`), and carry `age_days`/`stale`. `memento_list` is
compact by default (a table of contents: key/summary/state/age — the full
value is one `memento_get` away) and newest-activity-first; cheap partial
updates (dated note, status flip) go through `memento_patch` without
re-sending the value; `memento_add_batch` packs the session-end ritual into
one round-trip. Registration and
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
[docs/how-it-works.md](docs/how-it-works.md). Backup and restore (the standard
OpenSearch snapshot API): [docs/backup.md](docs/backup.md).

**Secrets do not belong in facts** (passwords, tokens, private keys) — a rule for
agents and humans, not a guarantee: the store is plaintext, and the
best-effort secret guard (default `reject`) scans writes for credential-like
text, but a secret with no known shape passes. The rule stays the first line
of defense.

## JS rendering (obscura, opt-in)

Some documentation sites answer a plain fetch with 403/422/429 — they gate
on the TLS/HTTP2 fingerprint, not the content. `fetch_render: "obscura"`
enables a fallback: when a plain fetch fails the way bot walls fail (network
walls, IP-ban statuses — not 404/401, not the allowlist, not oversize), the
URL goes to [obscura](https://github.com/h4ckf0r0day/obscura), a lightweight
headless browser with a Chrome-shaped stealth fingerprint. The rendered HTML
flows through the exact same extract + guard + store pipeline (`via:
obscura`).

It is opt-in by design: obscura is a third-party binary (~75 MB, linux
x86_64, glibc 2.35+) that **executes page JS locally**. Install it pinned:

```
make obscura   # sha256-pinned v0.2.6 stealth build → ~/.local/bin
```

then set `fetch_render: "obscura"` (env `MEMENTO_FETCH_RENDER`).
`fetch_obscura_path` points at the binary (default: `obscura` via PATH);
`fetch_obscura_stealth` (default `true`) passes `--stealth`.

Egress rules: the proxy (`fetch_proxy_url`, validated at config load — every
command, not just fetch) and the environment proxy are honored by the
renderer; a proxy it cannot forward fails the render loudly, never a direct
render behind it. A direct render resolves the target through the same
public-address rule as the plain fetch (no internal addresses). See
docs/setup.md §8 for the full egress and SSRF model.

v1 limits, stated plainly: the dump mode does not report the post-redirect
URL, so a rendered doc stores `final_url` = the requested URL; the renderer
is serialized (one render at a time); `robots.txt` (when
`fetch_respect_robots` is on) gates both the plain and the render path; the
URL is visible in the renderer's argv (`/proc`) — pages fetched with secrets
in query strings stay a plain-fetch concern.

## Platform notes

The Linux keyring provider (native D-Bus Secret Service) is the daily driver
and is exercised live. The darwin/windows providers (system Keychain /
Credential Manager via zalando/go-keyring) are compile-checked in CI
(cross-build) and the credential-resolution logic around them is
unit-tested with a fake provider, but the Keychain/Credential Manager
backends themselves are not exercised in CI and not smoke-tested on real
Apple/Windows hardware by the author. Release binaries cover linux/amd64,
linux/arm64, darwin/amd64, darwin/arm64 and windows/amd64; only linux/amd64
is smoke-tested by the author — reports from the other machines are welcome
and move this section. If the keyring misbehaves on your platform, open an
issue.

## License

[GPL-3.0](LICENSE)

