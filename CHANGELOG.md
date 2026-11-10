# Changelog

Notable changes per release tag. v0.6.0 is the pre-publication hardening
release; v0.1.0 was the first internal build.

## Unreleased

History journal: the in-doc `history` array is replaced by an append-only
journal of dated indices (`<index>-history-<period-date>`, monthly by default;
`history_index_period` config, `MEMENTO_HISTORY_PERIOD` env).

- Every accepted value is journaled before the fact doc is updated; the fact
  carries `current_ref` to its journal entry. The journal is write-forever
  (entries are never rewritten, no CAS); `history_limit` is now only the
  default `--keep` for the manual `memento prune`.
- New commands: `memento journal <key> [--limit N]`, `memento history-info
  <key>`, `memento migrate-history [--project p] [--yes]` (one-time move of
  pre-journal in-doc arrays); new MCP tools `memento_journal`,
  `memento_history_info` (13 tools).
- Journal infrastructure (server-clock ingest pipeline, index template) is
  created automatically on the first accepted write. Dated indices keep old
  periods cheap to inspect, snapshot, or delete; after an admin deletes old
  journal indices, read counts are lower bounds.
- Backups: snapshots must include `memento-history-*` (docs/backup.md).

Authorship and summary:
- Every write is stamped with self-reported audit labels: `author` (resolved
  from the store's security plugin, else config `author`, else the OS user),
  `author_source` (`security`/`config`/`os`/`none`), `model`, `host`,
  `session`. Birth labels are write-once; later writes move `updated_by`.
  Journal entries carry the writer who appended them.
- Long values stay skimmable: a value over `summary_min_len` (default 2000)
  requires a `summary` (CLI `--summary`, MCP `summary`) capped at
  `summary_max_len` (default 400). The rule covers accepted values; notes
  touches of old long facts are never rejected. `memento_search` matches
  summaries: the search fields are value, notes and summary.
- Config keys `author`/`model`/`host`/`session`/`summary_min_len`/
  `summary_max_len`; env `MEMENTO_AUTHOR`/`MEMENTO_MODEL`/`MEMENTO_HOST`/
  `MEMENTO_SESSION`/`MEMENTO_SUMMARY_MIN_LEN`/`MEMENTO_SUMMARY_MAX_LEN`.
  `memento status` reports the effective author and its chain.

MCP ergonomics (context economy, partial updates, batch):
- `memento_list` is **compact by default** (`compact: false` restores full
  rows) and **sorted by `updated_at` desc** by default (`sort: "key"` for the
  old stable order). A compact row carries key/summary/category/status/
  notes-head/age; the full value is one `memento_get` away (~5x tokens saved
  on the most expensive call in a session). `memento_keys` is always compact.
  **Breaking** for clients relying on full rows / key order.
- New `memento_patch`: partial update of an existing fact — value, summary,
  source, notes replace, atomic `notes_append` (dated line, applied
  server-side so concurrent writers cannot drop a note), status flip
  (a transition is journaled; obsolete→verified sets `observed_at=now`;
  `reason` goes with obsolete). On a value change the old summary is cleared
  unless a new one is provided (a stale table of contents is a lie).
- New `memento_add_batch` (1..10 facts per call, the session-end ritual in
  one round-trip): per-item guard and `confirm_secret`, duplicate keys
  rejected, partial success reported per item.
- `memento_get` accepts `keys: [...]` (up to 10) for batch reads
  (`items[]` with per-key `found`).
- `memento_add` answers say what happened: `created`, `changed` (false =
  same-value touch) and `previous_summary` (the superseded table of
  contents). `memento_obsolete`/status flips are journaled (audit symmetry).
- `memento_search_docs` hits carry the MATCHED section (`section: {order,
  heading, snippet}` centered on the match) — `memento_get_doc(section=order)`
  reads it in full; doc-level snippet remains the fallback.
- `memento_search`/`memento_list` zero-hit answers carry a navigation `hint`;
  `memento_search` gained `compact: true` for value-free rows.
- `memento_status` reports `mcp_version`, `commit`, `started_at` (the running
  binary, not the repo on disk); `version` renamed to `opensearch_version`.
  **Breaking** for clients reading `version`.
- 15 MCP tools.

Fetch guard triage and fetch v1 fixes:
- The secret guard now grades findings: `high` (vendor prefixes, PEM blocks,
  basic-auth URLs, `Bearer`, assignments with a 20+ char multi-class secret) vs
  `medium` (long hex/base64, weak assignments like `password = someVarName`).
  On fetched pages: high-confidence shapes in agent-visible fields and the
  stored HTML are REDACTED in place (marker `[memento-redacted:pattern]`) and
  the page is stored — the answer lists `redactions` (field/pattern/size, no
  preview) plus a `warning`; redaction runs before `size`/`content_hash`, so
  metadata matches the stored text. Medium findings in visible text refuse the
  write (confirmable at `ask`, no bypass at `reject`) — silent redaction would
  mangle real documentation. The same graded set scans fact writes.
- Default fetch User-Agent is a desktop Chrome string (sites 403 known bot
  UAs; override with `fetch_user_agent`/`MEMENTO_FETCH_USER_AGENT`).
- 429 is treated as an IP ban: with `fetch_proxy_fallback` the proxy is a new
  IP; without a proxy, one polite retry after a short pause. `Retry-After` is
  not honored (a hostile server could park the client).
- `text/markdown` / `text/x-markdown` responses extract as plain text (raw
  README endpoints are documentation); the request Accept advertises it.
- New config: `fetch_render` (`off`|`obscura`, default `off`),
  `fetch_obscura_path` (default `obscura`), `fetch_obscura_stealth`
  (default `true`); env `MEMENTO_FETCH_RENDER`, `MEMENTO_FETCH_OBSCURA_PATH`,
  `MEMENTO_FETCH_OBSCURA_STEALTH`. See JS rendering below.
- Docs: least-privilege store user guide for clusters with the security
  plugin (role/user/role-mapping recipes; the app role cannot delete the
  facts index; `indices:admin/get` + per-pattern create/settings/update
  actions included, with an upgrade note for roles provisioned before
  this release), linked from SECURITY.md.

Fixed:
- Journal infrastructure: a failed first install (403, network blip) is
  no longer remembered as "installed" — appends retry it until it lands,
  so a long-lived MCP server never journals without the template and the
  server-clock pipeline. Journal indices created while that was broken are
  healed: before appending, the app pins the ts pipeline on the index
  (idempotent settings PUT), so entries never sort as the oldest and
  become the first prune victims.
- `memento migrate-history` reruns are safe after a partial failure:
  entries are appended under content-derived ids, so the rerun collides
  with (and skips) what the previous run wrote instead of duplicating;
  a racing write's `current_ref` is never overwritten.
- `prune`: a transient fact-read failure aborts the prune instead of
  silently deleting the entry that carries the current value.
- Journal reads order by the store clock (`ingest_at`), not the writers'.
- Correction journal entries carry the fact's real `fact_id`.
- Negative `MEMENTO_SUMMARY_MIN_LEN`/`MEMENTO_SUMMARY_MAX_LEN` are
  rejected (they silently disabled the corresponding check); blank
  author labels are a loud config error, not a silent OS-user fallback.
- `list`/`search` lines show the summary (the table-of-contents
  contract); `get` shows a summary line; author labels are quoted in
  `get`/`journal`/`status` and stripped of non-printable characters.

## v0.6.0 — 2026-11-08

Internal pre-release. A six-round review cascade (Strata, DeepSeek Flash,
DeepSeek Pro) is complete: every finding — two High (a password echoed into
agent context, a quadratic HTML parser that let one hostile 2 MB page wedge
the server) and ~55 findings overall — is fixed, each with tests. `make ci`
is green.

Security
- SSRF: dial-time guard on every hop's resolved IP (DNS rebinding closed),
  redirect-chain checks, per-hop env-proxy re-evaluation; blocked ranges
  widened: CGNAT 100.64.0.0/10 (Tailscale), IETF reserved / documentation /
  benchmarking / class-E, plus 6to4 and Teredo embedded-IPv4 decoding.
- No credential echo on any error path (fetch, creds, config), including
  URL parse failures; userinfo URLs are refused everywhere, including
  `memento creds` and `opensearch_url`.
- Secret guard: `reject`/`ask`/`allow`; the stored raw HTML now redacts
  high-confidence credential shapes instead of rejecting whole pages
  (data-URI images and asset hashes no longer make normal pages
  uncacheable).
- Keyring on Linux: native D-Bus Secret Service provider (KDE Plasma 6 and
  GNOME): upsert semantics (no duplicate entries, rotation no longer reads
  the stale password), a deadline on every call, dismissed prompts are
  hard errors.
- Control bytes stripped after entity unescaping (scraped pages can no
  longer inject ANSI escapes into stored titles/sections).
- `fetch_allow_private` opt-in wired end to end (it now works for literal
  private hosts, not only for names that resolve private).

Robustness
- HTML extractor fully linear: the two remaining quadratic tail scans
  (unclosed drop tags, unterminated tags stepping one byte) are gone;
  regression tests run hostile inputs under a timing budget.
- Fact lifecycle: `category` follows a correction (it drives the staleness
  TTL), a fetch cache hit re-tags the doc's `project`, the destructive
  prune plan is a normal MCP result (an agent swallowing tool errors can
  no longer skip the safety barrier).
- Filters canonicalized at both write and read edges (status, category);
  an unknown status fails loudly instead of silently matching zero facts.
- One config load per command; the MCP server no longer resolves config
  through the working directory (no credential hijack via project dirs).

Tooling and docs
- Honest pagination everywhere: `truncated` and `skipped` signals in all
  CLI commands and MCP outputs (a cut answer never reads as complete).
- Release binaries for linux/amd64, linux/arm64, darwin/amd64,
  darwin/arm64 and windows/amd64 + sha256 checksums.
- `make dev-opensearch`: containerized single-node OpenSearch
  (podman/docker compose) for onboarding and dev loops.
- Docs: setup, how-it-works, MCP tool reference, backup/restore (verified
  live against the snapshot API, incl. the single-node replicas:0 trap),
  Dashboards bundle, agent rules (`rules/memento.md`); claims made honest
  (platform coverage, guard behavior, the quick-start config works as
  written).

## v0.1.0 — 2026-11-07

First internal release: the facts store over OpenSearch (provenance,
history, notes, per-category TTL staleness, verified/obsolete), CLI + MCP
server, page fetch with section extraction and a docs index, OS keyring
credentials with a plaintext opt-in, secret guard, Makefile, CI and
tag-driven release builds.
