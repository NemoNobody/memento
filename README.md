# memento

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
  overwriting it;
- **notes**: free-text qualifiers, e.g. "checked the dev config only".

Two statuses: `verified` (the subject exists) and `obsolete` (the subject is
gone — file deleted, endpoint removed). The store holds the evidence; the agent
judges trust and staleness in context.

## Quick start

Build (Go 1.21+):

```
go build -o memento .
```

Configure: copy `memento.conf.example` to `memento.conf` (or `~/.memento.conf`)
and fill in your OpenSearch connection.

```json
{
  "opensearch_url": "https://127.0.0.1:9200",
  "opensearch_user": "admin",
  "opensearch_password": "<your-password>",
  "insecure_skip_verify": true,
  "index_name": "memento"
}
```

The index is created automatically on first use. Env vars override the file:
`OPENSEARCH_URL`, `OPENSEARCH_USER`, `OPENSEARCH_PASSWORD`,
`OPENSEARCH_INSECURE_SKIP_VERIFY`, `MEMENTO_INDEX`; `MEMENTO_CONF` points at an
explicit config file. Check the connection:

```
memento status
```

Store and read back a fact:

```
memento add demo:api:port "API port is 8080" endpoint -p demo -s tool:read_file
memento get demo:api:port
memento search "api port"
```

## Commands

| Command | Description |
|---------|-------------|
| `memento add <key> <value> <category> [--project <p>] [--source <s>] [--notes <n>]` | Add or correct a fact (old value goes to history) |
| `memento get <key>` | Get a fact by key |
| `memento search <query> [--project <p>] [--category <c>] [--status <s>]` | Full-text search over values and notes |
| `memento list <project>` | List all facts for a project |
| `memento obsolete <key> [reason]` | Mark a fact obsolete because its subject is gone (reason appended to notes) |
| `memento semantic <query> [--top-k 5]` | Semantic search (needs an embedding model, not wired yet) |
| `memento status` | Check connectivity and show the effective configuration |
| `memento mcp` | Run as an MCP server over stdio |

Key convention: the key names a **stable subject**, not its current value —
`demo:api:port`, not `demo:api:v1`. Re-using the same key with a new value is a
correction that keeps the old value in history; re-adding the same value updates
notes only.

## MCP server

`memento mcp` speaks MCP over stdio, so agents (VS Code, others) call
`memento_status`, `memento_search`, `memento_get`, `memento_add`, `memento_list`
and `memento_obsolete` directly. Registration and troubleshooting:
[docs/mcp.md](docs/mcp.md).

## Storage

Facts live in a single OpenSearch index (default `memento`). One node is enough
to start; a proper cluster works the same way. How to point `memento` at a
cluster: [docs/setup.md](docs/setup.md). Schema and design:
[docs/how-it-works.md](docs/how-it-works.md).

**Never store secrets** (passwords, tokens, private keys): the index is plaintext.

## License

[GPL-3.0](LICENSE)

