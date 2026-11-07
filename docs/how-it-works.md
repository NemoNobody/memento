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
| `history` | object (`enabled: false`) | past observations `[{value, observed_at, source}]` — stored, never indexed |

### Key conventions

- `key` is the document ID. Re-adding the same key is an idempotent upsert:
  a new value moves the old one to `history`; the same value updates notes only.
- `key` should be stable, unique per fact, and written in **predictable English**
  (`project:area:location:detail`) — a later session must be able to guess it
  without searching.
- `value` is the human-readable fact.

## Commands

### add

```
memento add <key> <value> <category> [--project <p>] [--source <s>] [--notes <n>]
```

Adds a fact. If the key exists: a different value corrects it (the old value
moves to `history`); the same value updates `notes`/`source` only (no history
entry, `observed_at` stays). `--notes` carries qualifiers ("checked dev config
only") to later sessions.

### get

```
memento get <key> [--history]
```

Returns the fact for a key, plus the freshness signal (`age_days`, `stale`).
The history chain is **off by default** (it bloats the context of agents that
read facts on every turn); `--history` prints it.

### search

```
memento search <query> [--project <p>] [--category <c>] [--status <s>]
```

Full-text search over `value` and `notes`, filtered by project/category/status.
Default status is `verified`. Stale facts are marked `[stale: Nd]` in the
listing.

### list

```
memento list <project>
```

All facts for a project. Stale facts are marked `[stale: Nd]`.

### prune

```
memento prune <key> [--keep N] [--yes]
```

Drops history entries beyond the newest `N`, keeping the current value
untouched. `N` defaults to `history_limit` from the config.

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
`notes`. Use `add` instead when the value merely changed — corrections keep the
old value in `history`.

## History retention

Corrections are frequent (state keys get re-observed daily), so an unbounded
`history` array bloats every response. The correction script appends the old
value and then trims the oldest entries beyond `history_limit` (config key
`history_limit`, env `MEMENTO_HISTORY_LIMIT`, default 20; `0` keeps
everything). `memento prune` applies the same idea on demand, behind a
confirmation barrier.

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

## Auth

The CLI uses **Basic auth** (user + password). Connection and auth settings
come from a config file:

- `memento.conf` (current dir)
- `~/.memento.conf`
- `MEMENTO_CONF` env var

Env vars override the file: `OPENSEARCH_URL`, `OPENSEARCH_USER`,
`OPENSEARCH_PASSWORD`, `MEMENTO_INDEX`, `MEMENTO_HISTORY_LIMIT`.

OpenSearch also offers API keys, mTLS, and enterprise SSO (Kerberos / LDAP /
JWT), but `memento` speaks **Basic auth only** — user and password in the
config.

## Notes

- `insecure_skip_verify` is `true` by default because the local OpenSearch
  uses a self-signed cert. Set it to `false` for a trusted CA.
- The index is created automatically on first run.
