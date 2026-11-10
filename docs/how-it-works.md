# How memento works

## Storage model

Facts are stored in an OpenSearch index (`memento` by default). Each fact is a
document keyed by its `key` (the key is the document ID).

### Index schema

| Field | Type | Purpose |
|-------|------|---------|
| `fact_id` | keyword | stable doc id (UUID) |
| `key` | keyword | stable key, e.g. `proj:foo:loc:bar.py` |
| `value` | text | fact text (full-text searchable) |
| `category` | keyword | `location \| config \| endpoint \| behavior \| context_map` |
| `project` | keyword | `global` or project name |
| `status` | keyword | `verified \| obsolete` |
| `created_at` | date | creation timestamp |
| `updated_at` | date | last write timestamp (moves on any change, incl. notes) |
| `observed_at` | date | when the current value was seen (moves only on value change) |
| `source` | keyword | `tool:read_file \| experiment \| user:confirmed \| doc:*` |
| `notes` | text | free-text qualifiers (omitted when empty; obsolete reasons appended here) |
| `summary` | text | short table-of-contents rendering of the value; required when the value is over `summary_min_len` (see below) |
| `author` | keyword | who recorded the fact (self-reported label, write-once) |
| `author_source` | keyword | which chain link named the author: `security` / `config` / `os` / `none` |
| `model` | keyword | model label of the writer at creation (write-once, self-reported) |
| `host` | keyword | host label of the writer at creation (write-once, self-reported) |
| `session` | keyword | agent session label at creation (write-once, self-reported) |
| `updated_by` | keyword | self-reported label of the last writer (moves on every write) |
| `current_ref` | keyword | id of the journal entry that carries the current value |
| `history` | object (`enabled: false`) | **legacy** pre-journal array `[{value, observed_at, source}]` — stored, never indexed; `memento migrate-history` moves it into the journal and removes the field |

### Key conventions

- `key` is the document ID. Re-adding the same key is an idempotent upsert:
  a new value is appended to the journal and the fact points at the entry
  (`current_ref`); the same value updates notes only.
- `key` should be stable, unique per fact, and written in **predictable English**
  (`project:area:location:detail`) — a later session must be able to guess it
  without searching.
- `value` is the human-readable fact.

## Commands

### add

```
memento add <key> <value> <category> [--project <p>] [--source <s>] [--notes <n>] [--summary <s>]
```

Adds a fact. If the key exists: a different value corrects it (the accepted
value is appended to the journal, then the main doc is updated to point at the
entry); the same value updates `notes`/`source` only (no journal entry,
`observed_at` stays). `--notes` carries qualifiers ("checked the dev config
only") to later sessions. A value over `summary_min_len` requires `--summary`
(the store's table of contents, see below).

### get

```
memento get <key> [--history]
```

Returns the fact for a key, plus the freshness signal (`age_days`, `stale`).
The history chain is **off by default** (it bloats the context of agents that
read facts on every turn); `--history` prints the legacy in-doc array of
pre-journal facts. Journal-era history is read with `memento journal` /
`memento history-info`.

### search

```
memento search <query> [--project <p>] [--category <c>] [--status <s>]
```

Full-text search over `value`, `notes` and `summary`, filtered by project/category/status.
Default status is `verified`. Stale facts are marked `[stale: Nd]` in the
listing.

### list

```
memento list <project> [--prefix <p>]
```

All facts for a project, optionally narrowed to a key prefix (navigation cap
1000; the CLI says when the answer was truncated). Stale facts are marked
`[stale: Nd]`.

### journal / history-info

```
memento journal <key> [--limit N]
memento history-info <key>
```

`journal` reads the journal for a key: every accepted value, newest first
(`--limit` caps the page). `history-info` is the cheap overview: writes,
rediscoveries, first/last timestamps, `current_ref` — no entry bodies. Use it
to answer "did this fact change, how often, when last" before pulling the
full journal.

### prune

```
memento prune <key> [--keep N] [--yes]
```

Drops journal entries beyond the newest `N` (journal-first; keys that predate
the journal fall back to the legacy in-doc array), keeping the current value
untouched. `N` defaults to `history_limit` from the config. The journal is
write-forever by design, so pruning is a manual admin tool, never automatic.

Pruning is destructive (dropped observations have no undo), so it sits behind a
barrier: **without `--yes` nothing is written** — the command prints the plan
(how many entries exist, how many survive, how many are lost) and exits. An
agent must show that plan to the user and re-run with `--yes` only after their
explicit approval.

### obsolete

```
memento obsolete <key> [reason]
```

Marks the fact `obsolete` because its subject is gone (file deleted, endpoint
removed). The value and `observed_at` are kept; the reason is appended to
`notes`. Use `add` instead when the value merely changed — corrections journal
the new value.

## The journal (history)

History is an append-only **journal** of dated indices, not an array inside
the fact doc:

- One entry per accepted value: `fact_key`, `op` (`write`; `rediscovery` is
  reserved for same-value re-confirmation by a different author), `value`,
  `notes`, `source`, `created_at`, author fields. `op_type=create`: appends
  never overwrite each other, there is no CAS.
- Entries live in `<index>-history-<period-date>`: the period granularity is
  `history_index_period` (config; env `MEMENTO_HISTORY_PERIOD`; default
  `month` — `2026.11`; also `day`, `week` (`2026-w46`), `quarter`
  (`2026-q4`), `year`). Dated indices keep old periods small, inspectable,
  and deletable (the admin pruning path).
- The journal infrastructure (ingest pipeline `<index>-journal-ts` stamping
  entries with the **server clock** — writer clocks skew on a shared store;
  index template `<index>-journal` with explicit mappings) is created
  automatically on the first accepted write.
- Write order: the journal append happens **before** the main-doc update.
  The journal is the accepted log, the fact doc is the projection. A failed
  main write leaves an orphan entry (truth: the value was accepted, the
  projection is fixed by the next write); the reverse order could lose
  values from the log.
- The journal is never pruned automatically (write-forever): history shows
  how old it is. `history_limit` is only the default `--keep` for the manual
  `memento prune`.
- After an admin deleted old journal indices, read counts are lower bounds —
  the app cannot see deletions it did not perform.

### migrate-history

```
memento migrate-history [--project <p>] [--yes]
```

One-time upgrade step from pre-journal releases: moves each fact's in-doc
`history` array into the journal (entries land in the index of their
`observed_at`), appends the current value, sets `current_ref`, and removes
the array. It prints the plan first; `--yes` writes. Unmigrated facts keep
working; their history just stays in-doc until the next correction.
Reruns after a partial failure are safe: entries are appended under ids
derived from their content, so the rerun collides with what the previous
run wrote (collision = success) and never duplicates.

## History retention

Corrections are frequent (state keys get re-observed daily), so history is
kept out of the fact doc entirely: the journal carries it and fact reads stay
cheap. The journal is write-forever; `history_limit` (config key
`history_limit`, env `MEMENTO_HISTORY_LIMIT`, default 20) is the default
`--keep` for `memento prune`, which applies it on demand, behind a
confirmation barrier. Dated journal indices are the retention lever: an admin
can close, snapshot, or delete whole periods with standard OpenSearch tools.

## Staleness

The store provides the evidence (`observed_at`); the signal is computed at read
time, so it never rots in the index. Every read (`get`, `search`, `list`, and
the MCP tools) carries `age_days` and `stale`:

| Category | TTL (days) | Rationale |
|----------|-----------|-----------|
| `config`, `endpoint` | 14 | values move with deploys and config edits |
| `location`, `behavior` | 30 | files and code behavior move slower |
| `context_map`, unknown categories | never | decisions and design notes do not rot by age |

Rules:

- `stale` is `age_days > TTL`; at the exact TTL a fact is still fresh.
- A zero `observed_at` (facts predating the field) means `age_days: -1`; for
  TTL'd categories that is **stale** — the age cannot be trusted, so re-check
  the source. Categories without a TTL stay `stale: false`.
- `stale: true` is an instruction to re-verify, not a verdict to discard. The
  store provides the signal; the agent judges.
- Nothing in `add`/`obsolete` moves `observed_at` except a real value change, so
  a stale fact cannot be silently "re-freshed" by a notes touch.

## Summary (table of contents)

A long fact is useless in a listing: pulling every full value to find one is
exactly the context cost the store exists to avoid. So an accepted value over
`summary_min_len` runes (default 2000) must ship a `summary` — a short
table-of-contents rendering — and the summary itself is capped at
`summary_max_len` runes (default 400; ~100 tokens, about a 10x compression).
Both knobs are config keys (env `MEMENTO_SUMMARY_MIN_LEN` /
`MEMENTO_SUMMARY_MAX_LEN`); `0` disables the respective check.

The rule applies to the accepted value: create and correction. A notes-only
touch of a long fact that predates the rule is not rejected (the value is
unchanged), and a rejected correction never reaches the journal. The summary
is searchable text (`text`, BM25): `memento_search` can match it, which turns
the store into a skimmable index — find by summary, read the full value by
key.

## Authorship

Every write is stamped with self-reported labels: `author` (plus
`author_source` naming the chain link), `model`, `host`, `session`. The birth
labels are write-once; later writes move only `updated_by`, and every journal
entry carries the labels of the writer who appended it.

The author resolves once per process, down this chain:

1. `security` — the store's security plugin: `GET /_security/authinfo` (the
   name the *server* authenticated, so it cannot be forged by the client).
   Any non-success (security off, 403, timeout) silently degrades to the next
   link; a stalled plugin never slows writes (5s probe deadline).
2. `config` — the `author` config key / `MEMENTO_AUTHOR`.
3. `os` — the OS username of the process.
4. `none` — nothing resolved; writes carry no author labels.

`model`/`host`/`session` come from config keys / env (`MEMENTO_MODEL`,
`MEMENTO_HOST`, `MEMENTO_SESSION`); `host` falls back to the OS hostname
(sanitized), `model` and `session` are never guessed.

**Threat model:** these labels are audit metadata, not authentication. A
client can set any `author` it likes (config), and the `security` label is
only as strong as the cluster's per-user credentials. The purpose is
accountability in a cooperative shared store: who recorded what, when, which
model produced it — for review, drift analysis, and noticing that one source
is consistently wrong. On a hostile shared store, author labels inform a
human; they do not stop the hostile client (that is the store's security
plugin's job: per-user accounts and index permissions).

## Secret guard

The index is plaintext: a secret written once is exposed in every later read
(and in every agent context that reads it). The write paths (`add`,
`obsolete`, and the MCP tools) scan the user-supplied text — key, value,
summary, notes, source (for `obsolete`: key and reason) — for credential-like
shapes: vendor token
prefixes (`AKIA…`, `ghp_…`, `xox…`, `sk-…`, JWT, PEM headers), `Bearer`
headers, `password = …` assignments, and long opaque strings (hex/base64).

The level is `secret_guard` in the config (env `MEMENTO_SECRET_GUARD`). Every
finding carries a confidence level — `high` for deterministic credential
shapes (vendor prefixes, PEM key blocks, `Bearer` headers, basic-auth URLs,
assignments whose right-hand side is 20+ characters spanning 3+ character
classes) and `medium` for the generic shapes that also fire on ordinary
content (long hex/base64 like git SHAs and asset hashes, weak assignments
such as `password = someVarName`):

| Level | Behavior |
|-------|----------|
| `reject` (default) | credential-like text is never stored; there is no confirmation flag |
| `ask` | the write is refused until the caller confirms the finding is a false positive: `--allow-secret` (CLI) / `confirm_secret: true` (MCP) — an agent must show the finding to the user first |
| `allow` | scanning is off; a deliberate opt-out for shared stores |

Guarantees and limits:

- The guard runs **before any store contact**: a rejected write is rejected
  even when OpenSearch is down.
- The guard's message carries masked previews only (first 4 characters and
  the length) — the matched text is never echoed back into the caller's
  context. Echoing it would leak the secret a second time.
- Detection is regex-based best effort: a secret without a recognizable shape
  passes. A false positive is possible too — a 40-character git SHA in notes
  matches `long-hex` (medium); at `ask` the confirmation flag is the escape
  hatch.
- The fetched-page path uses the confidence split as policy: high-confidence
  shapes in the agent-visible fields (title, section text/headings, image
  alt/title) and in the stored raw HTML are REDACTED in place (`[memento-
  redacted:pattern]`) and the page is stored — the answer lists `redactions`
  (field/pattern/size, never a preview). Medium findings in the agent-visible
  text REFUSE the write (they fire on ordinary documentation too; silent
  redaction would mangle it): at `ask` a finding can be confirmed, at
  `reject` there is no bypass. Redaction runs before the stored `size` and
  `content_hash` are computed, so the metadata describes exactly the text
  that ships.
- Deliberate tradeoff: a high-confidence secret on a fetched page ends up
  stored in redacted form — the redaction must therefore out-cover a reject.
  For the shared plaintext docs store this is the accepted product decision:
  the secret never ships to agents, the documentation survives.
- The guard gates every route equally: a page rendered through the obscura
  fallback (README, "JS rendering") flows through the same hook — the
  renderer changes how the HTML arrives, not what is allowed into the store.

## Auth

The CLI uses **Basic auth** (user + password). Connection and auth settings
come from a config file:

- `--conf <path>` flag (`memento mcp` and `memento status`)
- `MEMENTO_CONF` env var
- `memento.conf` (current dir)
- `memento.conf` next to the binary
- `~/.memento.conf`

Env vars override the file: `OPENSEARCH_URL`, `OPENSEARCH_USER`,
`OPENSEARCH_PASSWORD`, `OPENSEARCH_INSECURE_SKIP_VERIFY`, `MEMENTO_CONF`,
`MEMENTO_INDEX`, `MEMENTO_HISTORY_PERIOD`, `MEMENTO_HISTORY_LIMIT`,
`MEMENTO_SECRET_GUARD`,
`MEMENTO_ALLOW_PLAINTEXT`, `MEMENTO_SUMMARY_MIN_LEN`,
`MEMENTO_SUMMARY_MAX_LEN`, `MEMENTO_AUTHOR`, `MEMENTO_MODEL`,
`MEMENTO_HOST`, `MEMENTO_SESSION`, plus the `MEMENTO_FETCH_*` /
`MEMENTO_DOCS_*` / `MEMENTO_LOCAL_REPO_ROOTS` / `MEMENTO_REPO_SCAN_DEPTH`
fetch options (the full list lives in README and docs/setup.md).

OpenSearch also offers API keys, mTLS, and enterprise SSO (Kerberos / LDAP /
JWT), but `memento` speaks **Basic auth only** — user and password in the
config.

## Notes

- `insecure_skip_verify` defaults to `false`: TLS certificates are verified.
  The local OpenSearch uses a self-signed cert, so a local setup sets it to
  `true` explicitly. Never enable it for a shared network.
- The index is created automatically on first run. The journal infrastructure
  (pipeline, template, dated journal indices) is created on the first
  accepted write.
