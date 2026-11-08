# MCP server

`memento` can run as an MCP server, so an agent calls the store directly —
searching before re-verifying something and storing what it verified — instead of
you running the CLI by hand.

```
memento mcp [--conf <path>]
```

It speaks MCP over **stdio**: JSON-RPC on stdin/stdout, diagnostics on stderr.
The client (VS Code) starts it; you never run it manually except to debug.

## Registering it

memento is meant to be available in **every** project (it is memory, not a project
setting), so prefer user-wide registration. How a config is picked up depends on
the session type:

| Scope | File | Read by |
|---|---|---|
| User, portable | `~/.copilot/mcp-config.json` (`$COPILOT_HOME` if set) | the Agent Host and the Copilot CLI **natively**; regular chat only when `chat.mcp.discovery.enabled` enables the `copilot` source — all discovery sources are **off by default** |
| VS Code user profile | `mcp.json` via `MCP: Open User Configuration`, or `code --add-mcp <json>` | regular chat natively; VS Code marks this destination as deprecated for compatibility |
| Workspace, portable | `.mcp.json` at the project root | regular chat, that project only |
| Workspace, VS Code | `.vscode/mcp.json` | deprecated for compatibility |

**A registered server is not necessarily a running one** — a server that never
started is invisible to the agent. Verify with `MCP: List Servers`, or read the
per-server log `~/.config/Code/logs/<stamp>/window1/mcpServer.<source>.memento.log`
(`workspace-dot-mcp` = workspace `.mcp.json`; `copilot` = the discovered user
config). Newly registered servers start on the next chat message
(`chat.mcp.autostart`) or on a window reload — and non-workspace servers also
need a **trust approval** (approve the trust dialog / add the server to the
trusted list): until it is approved, the server stays stopped and its log stays
empty. VS Code asks again whenever the server definition changes — expect a
prompt while iterating on the config; rebuilding the `memento` binary alone does
not change the definition.

A registration in `~/.copilot/mcp-config.json`:

```json
{
  "mcpServers": {
    "memento": {
      "type": "stdio",
      "command": "<path-to-binary>/memento",
      "args": ["mcp", "--conf", "<path-to-binary>/memento.conf"]
    }
  }
}
```

For regular chat to read that file, discovery must be enabled for the `copilot`
source (VS Code 1.138+):

```json
// user settings.json
"chat.mcp.discovery.enabled": {
    "claude-desktop": false,
    "copilot": true,
    "windsurf": false,
    "cursor-global": false,
    "cursor-workspace": false
}
```

Register the server in exactly **one** scope: on a name collision VS Code applies
`chat.mcp.collisionBehavior` (default `disable`; set it to `suffix` to keep both
under distinct names) — do not keep a workspace `.mcp.json` and the user-wide
file for the same server name.

Two details that matter:

- **Absolute path to the binary.** MCP clients do not run your shell profile, so
  `PATH` is not a reliable place to look. If you install `memento` somewhere else,
  update this path.
- **`--conf` with an absolute path.** The server is started from an arbitrary
  working directory, so `./memento.conf` is unreliable. Without `--conf` the MCP
  lookup chain is `MEMENTO_CONF` → next to the binary → `~/.memento.conf`; the
  working-directory entry that the plain CLI uses is skipped on purpose — a cloned
  repository's `memento.conf` must never hijack where your facts and credentials
  are sent. `--conf` removes the guesswork.

Do not put the OpenSearch password in the MCP config: it resolves from the OS
keyring (`memento creds set`), `OPENSEARCH_PASSWORD`, or `memento.conf` (the
latter only when `allow_plaintext` is on).

## Tools

| Tool | Purpose | Notes |
|---|---|---|
| `memento_status` | Is the store reachable? What is the effective config and storage state? | **Never fails** — `ok=false` plus a hint instead, so the agent can explain the problem; includes `password_from` (`env`/`keyring`/`file`/`none`, never the value) |
| `memento_search` | Full-text search over fact values and notes | Trusted facts only by default; `status: "any"` includes obsolete ones; history off unless `include_history: true` |
| `memento_get` | Exact fact by key | `found: false` when absent — not an error; history off unless `include_history: true` |
| `memento_add` | Store a verified fact | Shape is validated before the guard and the store: non-empty `key`/`value`, and `category` one of `location`/`config`/`endpoint`/`behavior`/`context_map`, stored lowercase (an unknown category never ages — silent rot). Corrects by key: a new value moves the old one to history (trimmed to `history_limit`); the same value updates notes/source only. `notes` are replaced only by a non-empty new note (an omitted `notes` leaves the stored qualifiers alone — they never enter history, so erasing them is irreversible); `secret_guard` rejects credential-like text before any store contact |
| `memento_list` | Facts of a project | Same trust default as search; `prefix` keeps only keys starting with it (trailing `*` allowed); history off unless `include_history: true` |
| `memento_keys` | Find facts by key across all projects | `prefix` (key start) and/or `match` (key substring; `*`, `?` wildcards); at least one required; sorted by key; `project` is an optional filter; key matching is case-sensitive (keys conventionally lowercase) |
| `memento_prune` | Drop old history entries under a key, keeping the newest `keep` | **DESTRUCTIVE** — without `confirm: true` nothing is written, the call returns the plan; show it to the user and re-call only after their approval |
| `memento_obsolete` | Mark a fact obsolete because its subject is gone | Value is kept for reference; the reason is appended to notes and passes the `secret_guard`. Use `memento_add` when the value merely changed |
| `memento_fetch_doc` | Fetch a web page into the docs index, return its outline (hash + sections) | Cache-aware: a second fetch of the same URL returns the stored snapshot (`cached: true`) until its TTL; `refresh: true` forces a download. Requests send no cookies and no credentials; a host outside `fetch_allow_hosts` is rejected before the request. Internal addresses (loopback, private, link-local incl. cloud metadata) are refused at every hop and at dial time — the SSRF guard; `fetch_allow_private: true` is the opt-in for self-hosted docs on a LAN. GitHub URLs that match a local clone resolve to a local path (`local: true`) with no fetch and no store write. The `secret_guard` scans the extracted text before the store write |
| `memento_get_doc` | Read a stored snapshot by hash: outline / `section: N` text / `full: true` | Never touches the network; `found: false` is an answer, not an error; `stale: true` means the snapshot is past its TTL — refresh with `memento_fetch_doc` before quoting. Prefer one section: `full` bloats the context |
| `memento_search_docs` | BM25 search over stored page sections | Returns hash+url+title+snippet per doc (default limit 5, cap 20); the raw html is write-only and never searched; pull text with `memento_get_doc` |

Note: `memento_search` does **not** match keys — use `memento_get` for an exact
key, `memento_keys` for key navigation (prefix/substring across projects).

Facts tools answer with `count` and the honest truncation signal: `truncated:
true` means the cluster matched more facts than this page delivered — for
`memento_search` the page is the requested `limit` (raise it, max 50, before
narrowing the query); for `memento_list`/`memento_keys` it is the navigation
cap (1000) — narrow with prefix/project/status. A page exactly filling its cap
is a complete result. `skipped: N` means N stored hits failed to decode
(index corruption): report it to the user; the answer is incomplete, never
silently.

The `secret_guard` (config, default `reject`) scans `memento_add` and
`memento_obsolete` writes for credential-like text and refuses them before any
store contact; findings come back as masked previews (the secret is never
 echoed). At `ask`, an agent that has shown the finding to the user and got
explicit approval re-calls with `confirm_secret: true`; at `reject` there is no
bypass.

Every read carries `age_days` and `stale` (per-category TTL: `config`/`endpoint`
14 days, `location`/`behavior` 30 days, `context_map` never; writes reject
unknown categories, so a typo cannot create facts that rot in the dark).
`stale: true` means re-verify against the source before trusting the
fact — it is a signal, not a verdict.

The store tools deliberately **fail** when OpenSearch is unreachable, while
`memento_status` reports instead of failing. An empty result must never be mistaken
for "no such fact": that is what would make an agent re-verify everything it
already knew.

An importable rule for agents — when to consult and update memory — is in
[rules/memento.md](../rules/memento.md).

## Verify it works

1. Command Palette → `MCP: List Servers` — `memento` should be listed and running.
   If it is missing, reload the window.
2. In chat, ask the agent to call `memento_status`. Expect `ok: true`, the cluster
   name and version, the config file actually used, and storage state.
3. Store and read back a fact:
   - "Remember that the API port for the demo project is 8080 — use memento."
   - then, in a **new chat session**: "What is the API port for the demo project?"

Step 3 is the real acceptance test: the second session must answer without
re-verifying anything.

If the server shows an error, run `MCP: List Servers` → select `memento` → `Show
Output`. The same diagnostics are available from a shell as `memento status`.

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `memento` missing from the server list | Config not read **or server not started** — check `MCP: List Servers`, the server log (see "Registering it"), and whether discovery is enabled for the user-wide file |
| Registered, but the server never starts | Waiting for a chat-message autostart pass or a trust confirmation — or it collided with another `memento` registration (same name in two scopes) |
| Server starts, tools return `ok=false` | OpenSearch unreachable — the hint names the URL and the config file used. See `docs/setup.md` |
| `status` shows a config file you did not expect | The lookup chain found another file; pass `--conf` explicitly |
| Tools work in one project but not another | The registration is user-wide; check the MCP output log for a start-up error |
| Writes fail with "the cluster is blocking writes" | Cluster-side block (disk flood stage or a read-only index). Free space or ask the cluster owner; see `docs/setup.md` |
