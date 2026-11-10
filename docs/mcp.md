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
| `memento_status` | Is the store reachable? What is the effective config and storage state? | **Never fails** — `ok=false` plus a hint instead, so the agent can explain the problem; includes `password_from` (`env`/`keyring`/`file`/`none`, never the value), `opensearch_version`, and the running binary's `mcp_version`/`commit`/`started_at` (an agent can verify which build answers — the repo on disk is not the running server) |
| `memento_search` | Full-text search over fact values, notes and summaries | Trusted facts only by default; `status: "any"` includes obsolete ones; history off unless `include_history: true`; **compact by default** (`compact: false` restores full rows): key/summary/category/status/notes-head/age rows, the full value is one `memento_get` away; a non-compact-zero answer names that in `hint`, and a zero-hit answer carries a navigation `hint` |
| `memento_get` | Exact fact by key, or a batch via `keys: [...]` (up to 10, `items[]` with per-key `found`) | `found: false` when absent — not an error; history off unless `include_history: true`; carries `summary`, `author`/`author_source`, `updated_by` when present |
| `memento_add` | Store a verified fact | Shape is validated before the guard and the store: non-empty `key`/`value`, and `category` one of `location`/`config`/`endpoint`/`behavior`/`context_map`, stored lowercase (an unknown category never ages — silent rot). Corrects by key: the accepted value is appended to the journal and the fact points at the entry; the same value updates notes/source only and leaves the journal untouched. The answer says what happened: `created`, `changed` (false = same-value touch), `previous_summary` (the superseded table of contents). `notes` are replaced only by a non-empty new note (an omitted `notes` leaves the stored qualifiers alone — they never enter history, so erasing them is irreversible). A value over `summary_min_len` requires `summary` (under `summary_max_len`) — the store's table of contents. Writes are stamped with authorship labels (`author`/`author_source`/`model`/`host`/`session` — self-reported audit metadata, birth labels write-once; later writes move `updated_by`). `secret_guard` rejects credential-like text before any store contact. For cheap partial updates (dated note, status flip) use `memento_patch` — no value resend |
| `memento_add_batch` | Store 1..10 facts in one call (the session-end ritual in a single round-trip) | Per-item guard and `confirm_secret` (one approved false positive never whitewashes the rest); duplicate keys are rejected (the whole batch — a dupe makes created/changed order-dependent and ambiguous); partial success is legal: per-item `{key, created, changed, status, error?}` |
| `memento_patch` | Partial update of an existing fact (never creates) | Fields: `value`, `summary`, `notes` (replace), `notes_append` (dated line appended atomically server-side — a concurrent add cannot drop it), `source`, `status` (`verified`\|`obsolete`; a transition is journaled; obsolete→verified sets `observed_at=now`), `reason` (with obsolete). Omitted fields are untouched. On a value change the old summary is CLEARED unless a new one is provided (a stale table of contents is a lie); a long new value requires a new summary (the add rule) |
| `memento_patch_batch` | Patch 1..10 existing facts in one call (the session-end ritual when the facts already exist) | Per-item guard and `confirm_secret` (one approved false positive never whitewashes the rest); duplicate keys rejected (the whole batch); partial success legal: per-item `{key, changed, status, error?}`. Same field semantics as `memento_patch`; never creates (a missing key is a per-item store error) |
| `memento_list` | Facts of a project | Same trust default as search; **compact by default** (`compact: false` restores full rows): key/summary/category/status/notes-head/age rows — full values cost ~5x tokens and a project listing is the most expensive agent call; **sorted by `updated_at` desc** by default (`sort: "key"` for stable navigation; a same-value touch moves `updated_at` without changing `age`/`stale`); `prefix` keeps only keys starting with it (trailing `*` allowed); history ships only on full rows (`include_history: true` with `compact: false` — compact rows are navigation; `memento_journal` reads history); a zero-hit answer carries a navigation `hint` |
| `memento_keys` | Find facts by key across all projects | `prefix` (key start) and/or `match` (key substring; `*`, `?` wildcards); at least one required; sorted by key; `project` is an optional filter; key matching is case-sensitive (keys conventionally lowercase); rows are always compact (navigation is scanned, not read) |
| `memento_journal` | Read the journal for a key: the append-only history of accepted values, newest first (`limit`, default 20) | `op: write` is an accepted value, `op: rediscovery` is reserved for same-value re-confirmation by a different author; counts after an admin deleted old journal indices are lower bounds. Prefer `memento_history_info` first, the full journal only when the history matters |
| `memento_history_info` | Compact per-key history overview: writes, rediscoveries, first/last timestamp, `current_ref` — no entry bodies | Answers "did this fact change, how often, when last" cheaply; counts are LOWER BOUNDS (admin index pruning is invisible to the app) |
| `memento_prune` | Drop old journal entries under a key, keeping the newest `keep` | **DESTRUCTIVE** — without `confirm: true` nothing is written, the call returns the plan; show it to the user and re-call only after their approval. Journal-first; keys that predate the journal fall back to the legacy in-doc array. The journal is write-forever by design: pruning is a manual admin decision, never automatic |
| `memento_obsolete` | Mark a fact obsolete because its subject is gone | Value is kept for reference; the reason is appended to notes and passes the `secret_guard`; the transition is journaled. Use `memento_add` when the value changed, `memento_patch` to flip status |
| `memento_fetch_doc` | Fetch a web page into the docs index, return its outline (hash + sections) | Cache-aware: a second fetch of the same URL returns the stored snapshot (`cached: true`) until its TTL; `refresh: true` forces a download; passing a different `project` re-tags the stored snapshot (the tag is guard-scanned at the call, both paths). Requests send no cookies and no credentials; a host outside `fetch_allow_hosts` is rejected before the request. Internal addresses (loopback, private, link-local incl. cloud metadata) are refused at every hop and at dial time — the SSRF guard; `fetch_allow_private: true` is the opt-in for self-hosted docs on a LAN. GitHub URLs that match a local clone resolve to a local path (`local: true`) with no fetch and no store write. With `fetch_render: "obscura"` (opt-in), a fetch refused by bot walls (network failure, 403/422/429) is retried through the obscura headless browser and stored `via: obscura` (v1 limit: `final_url` = the requested URL); non-eligible failures (404/401, allowlist, redirect limits, oversize) stay plain. The `secret_guard` runs before the doc is finalized: high-confidence credential shapes in the agent-visible fields, the stored raw HTML and image hrefs are redacted in place (the answer lists `redactions` + a `warning`, no previews; redaction precedes `size`/`content_hash`). Medium findings route on the stored `content_type`: documentation types (html/markdown/plain) do not refuse the write — the shapes are ordinary content there — instead the answer surfaces `guard_medium_ignored: N` when N>0; API types (`application/json|xml`, `+json`/`+xml`) keep refusing (confirmable at `ask`). The URL and the post-redirect `final_url` are identity — scanned high-confidence only, and a credential-bearing redirect target refuses the doc. A direct render (no proxy) resolves the target and refuses non-public addresses (the render is the SSRF gate for its own route; with a proxy the proxy enforces the policy) |
| `memento_get_doc` | Read a stored snapshot by hash: outline / `section: N` text / `full: true` | Never touches the network; `found: false` is an answer, not an error; `stale: true` means the snapshot is past its TTL — refresh with `memento_fetch_doc` before quoting. Prefer one section: `full` bloats the context |
| `memento_search_docs` | BM25 search over stored page sections | Returns hash+url+title+project per doc with the MATCHED section (`section: {order, heading, snippet}` — the snippet is centered on the match; `memento_get_doc(section=order)` reads the section in full); doc-level snippet is the fallback when no section is identified; default limit 5, cap 20; the raw html is write-only and never searched |

Note: `memento_search` does **not** match keys — use `memento_get` for an exact
key, `memento_keys` for key navigation (prefix/substring across projects).
`include_history: true` on fact reads returns the legacy in-doc history array
(pre-journal facts); journal-era history is read with `memento_journal` /
`memento_history_info`.

Facts tools answer with `count` and the honest truncation signal: `truncated:
true` means the cluster matched more facts than this page delivered — for
`memento_search` the page is the requested `limit` (raise it, max 50, before
narrowing the query); for `memento_list`/`memento_keys` it is the navigation
cap (1000) — narrow with prefix/project/status. A page exactly filling its cap
is a complete result. `skipped: N` means N stored hits failed to decode
(index corruption): report it to the user; the answer is incomplete, never
silently. `memento_search_docs` answers with the same signals: `truncated: true`
means the docs index holds more matches than this page (cap 20) — narrow the
query before concluding there is nothing more.

The `secret_guard` (config, default `reject`) scans `memento_add`,
`memento_add_batch` (per item), `memento_patch` and `memento_obsolete` writes
for credential-like text and refuses them before any
store contact; findings come back as masked previews (the secret is never
 echoed). At `ask`, an agent that has shown the finding to the user and got
explicit approval re-calls with `confirm_secret: true`; at `reject` there is no
bypass.

Every read carries `age_days`, `age_hours` and `stale` (per-category TTL:
`config`/`endpoint` 14 days, `location`/`behavior` 30 days, `context_map` never;
writes reject unknown categories, so a typo cannot create facts that rot in the
dark). `age_hours` disambiguates fresh facts that all share `age_days: 0`.  
`stale: true` means re-verify against the source before trusting the
fact — it is a signal, not a verdict.

A value over `summary_min_len` characters must ship a `summary` (capped at
`summary_max_len`): a skimmable table of contents, so listings do not force
full values into the context. The rule applies to accepted values (new facts
and corrections); a notes-only touch of an old long fact is not rejected.

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
   name and version, the config file actually used, storage state, and the
   `author` label new writes will carry (with its chain: security/config/os).
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
