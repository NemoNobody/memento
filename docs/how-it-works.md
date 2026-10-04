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

Note: `embedding` is **not** part of the current mapping — the knn plugin is not
registered. It is reserved for semantic search (see PLAN §4.8).

### Key conventions

- `key` is the document ID. Re-adding the same key is an idempotent upsert:
  a new value moves the old one to `history`; the same value updates notes only.
- `key` should be stable and unique per fact: `project:area:location:detail`.
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
memento get <key>
```

Returns the fact for a key.

### search

```
memento search <query> [--project <p>] [--category <c>] [--status <s>]
```

Full-text search over `value` and `notes`, filtered by project/category/status.
Default status is `verified`.

### list

```
memento list <project>
```

All facts for a project.

### obsolete

```
memento obsolete <key> [reason]
```

Marks the fact `obsolete` because its subject is gone (file deleted, endpoint
removed). The value and `observed_at` are kept; the reason is appended to
`notes`. Use `add` instead when the value merely changed — corrections keep the
old value in `history`.

### semantic

```
memento semantic <query> [--top-k 5]
```

Semantic search. Requires an embedding model; not wired yet.

## Auth

The CLI uses **Basic auth** (user + password) by default. Connection and
auth settings come from a config file:

- `memento.conf` (current dir)
- `~/.memento.conf`
- `MEMENTO_CONF` env var

Env vars override the file: `OPENSEARCH_URL`, `OPENSEARCH_USER`,
`OPENSEARCH_PASSWORD`, `MEMENTO_INDEX`.

### Other auth options (OpenSearch)

OpenSearch supports several auth mechanisms. The CLI currently uses Basic
auth; the others are available if you need them:

- **API keys (tokens).** Short-lived (max 90 days), created via
  `POST /_plugins/_security/api/apitokens`. The token is returned once and
  must be rotated. Good for CI / short-lived jobs, less convenient for a
  long-lived CLI.
- **mTLS.** Client certificates; no password in the config. Most secure,
  more setup.
- **Kerberos / LDAP / JWT.** For enterprise / SSO setups.

For a long-lived local CLI, Basic auth via a config file is the simplest
and most practical.

## Notes

- `insecure_skip_verify` is `true` by default because the local OpenSearch
  uses a self-signed cert. Set it to `false` for a trusted CA.
- The index is created automatically on first run (`EnsureIndex`).
