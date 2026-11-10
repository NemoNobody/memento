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
- Keys are trimmed at every edge (add/patch/get/keys/journal/obsolete/prune;
  the batch dupe check uses the trimmed key): `" k "` and `"k"` never become
  two docs. A compact answer never ships history (full rows only).
- 15 MCP tools.

Fetch guard triage and fetch v1 fixes:
- The secret guard now grades findings: `high` (vendor prefixes, PEM blocks,
  basic-auth URLs, `Bearer`, assignments with a 20+ char multi-class secret) vs
  `medium` (long hex/base64, weak assignments like `password = someVarName`).
  On fetched pages: high-confidence shapes in agent-visible fields are REDACTED
  in place (marker `[memento-redacted:pattern]`) and the page is stored — the
  answer lists `redactions` (field/pattern/size, no preview) plus a `warning`;
  redaction runs before `size`/`content_hash`, so metadata matches the stored
  text. Medium findings in visible text refuse the write (confirmable at `ask`,
  no bypass at `reject`) — silent redaction would mangle real documentation.
  The URL and the post-redirect `final_url` are scanned high-confidence only
  (identity fields: a git SHA in a GitHub path is ordinary; a
  credential-bearing redirect target refuses the doc); image hrefs are
  redacted in place; the credential-assignment rule reads quoted JSON/YAML
  config lines (`"api_key": "S3cr3t"`); the agent-supplied `project` tag is
  guard-scanned on every write path (facts and docs — for cached docs at the
  call, the re-tag path never sees the content guard).
- The stored raw HTML is redacted with the high-confidence set, twice: the
  second pass closes the common overlap-tail case (a span starting inside an
  earlier replacement; the marker rule refuses a right-hand side starting with
  `[`, so the pair is a fixed point). A greedy span can still swallow a scheme
  prefix — the residual exposure is disk-only, the stored HTML is never served
  to agents. Its redactions join the audit trail as field `html`.
- `memento obsolete` refuses a reason on an already-obsolete fact (mirrors
  `memento_patch`): the journal skips a re-retire, so notes must not grow an
  audit line the journal never recorded.
- Keys, project labels and doc hashes are trimmed at every CLI and MCP edge
  (`add`, `get`, `obsolete`, `prune`, `journal`, `history-info`, `list`,
  `search`, `keys --match/--project`, `doc`; MCP read filters and the MCP
  project default included) — a padded filter never answers a silent zero.
- A same-value `add` on an obsolete fact resurrects it with a journal entry
  and moves `observed_at` (the Patch resurrect rule kept symmetric): the
  status flip is never silent. The MCP `memento_obsolete` guard now scans the
  key as well as the reason (CLI parity).
- Retire reasons fold to one line in notes (whitespace runs → single spaces):
  the audit trail stays one event per line.
- Render env-proxy hardening: the proxy Go's environment machinery produced
  is validated as a parsed struct — a malformed env proxy normalized so the
  credentials hide in the path is refused, and no `@` ever reaches obscura's
  argv (`/proc`-visible): the `@`/`%40` path check guards both the config and
  the env branch, and the SSRF pre-check is documented as guarding the
  requested URL only (obscura follows redirects itself — run it behind a
  proxy for per-hop policy).
- `add` never erases a stored `source` with an empty one (the notes rule):
  the qualifier is not kept in history on the touch path, so the wipe would
  be irreversible; a correction replaces source only when one is given.
- The guard's `redactions` audit lists only the spans the text actually lost
  (a match folded into an overlapping one is no longer reported), and the
  double redaction pass now covers every agent-visible field, not just the
  stored HTML.
- SSRF tunnels: the NAT64 well-known prefix (`64:ff9b::/96`, and its local
  `/48` variant `64:ff9b:1::/48`) is decoded to its embedded IPv4 like 6to4
  and Teredo already were; arbitrary RFC 6052 prefixes stay a documented
  residual.
- A correction journals the MERGED accepted state (the new value with the
  stored `source`/`notes` when the request left them empty): the journal
  entry and the doc never disagree about the qualifiers. The entry also
  repeats the doc's write-once `project` (a correction with the default
  `global` no longer journals a project the fact never had), and a
  resurrected fact journals the canonicalized category the doc received.
  `memento keys "k *"` normalizes to `k` once (the guard-checked prefix is
  what the wire sees), `memento_list`/`memento_search`/`memento_keys` and the
  CLI `search`/`keys` validate status (and `search --limit >= 0`) before any
  store contact, and `memento status`'s env list is now pinned against the
  loader's source (the "kept in sync" comment is a test, not a promise).
- `memento keys` shape errors answer before any store contact: a prefix that
  normalizes to empty (" " or "*") with no `--match` is a caller error, not
  a wrapped "the store is unreachable" advisory. `memento fetch --project`
  and `memento docs --project` are trimmed like every other edge, `memento
  status` lists the render env overrides (`MEMENTO_FETCH_RENDER`,
  `MEMENTO_FETCH_OBSCURA_PATH`, `MEMENTO_FETCH_OBSCURA_STEALTH`), and
  `memento prune` on legacy keys reports the actually removed count instead
  of a pre-read guess.

- Default fetch User-Agent is a desktop Chrome string (sites 403 known bot
  UAs; override with `fetch_user_agent`/`MEMENTO_FETCH_USER_AGENT`).
- 429 is treated as an IP ban: with `fetch_proxy_fallback` the proxy is a new
  IP; without a proxy, one polite retry after a short pause. `Retry-After` is
  not honored (a hostile server could park the client).
- `text/markdown` / `text/x-markdown` responses extract as plain text (raw
  README endpoints are documentation); the request Accept advertises it.
- New config keys `fetch_render` (`off`|`obscura`, default `off`),
  `fetch_obscura_path` (default `obscura`), `fetch_obscura_stealth`
  (default `true`); env `MEMENTO_FETCH_RENDER`, `MEMENTO_FETCH_OBSCURA_PATH`,
  `MEMENTO_FETCH_OBSCURA_STEALTH`.

JS rendering (opt-in, obscura):
- `fetch_render: "obscura"` adds a browser-render fallback for fetches refused
  the way bot walls refuse (network failures, 403/422/429 — NOT 404/401, not
  allowlist refusals, redirect limits, oversize): the URL goes to the
  [obscura](https://github.com/h4ckf0r0day/obscura) headless browser
  (Chrome-shaped stealth TLS/HTTP2 fingerprint), and the rendered HTML flows
  through the same extract + guard + store pipeline; the doc is stored
  `via: obscura`. Render failures surface BOTH reasons (plain + renderer).
- Third-party by design: ~75 MB binary, executes page JS locally, linux
  x86_64 glibc 2.35+. `make obscura` installs the sha256-pinned 0.2.6
  stealth build into `~/.local/bin`. The renderer is serialized (one render
  at a time, cancel-aware); `robots.txt` (when respected) gates both paths;
  the renderer keeps the operator's egress — `fetch_proxy_url` is validated
  at config load (every command fails on a bad value: the proxy is a route,
  not a fetch decoration), the environment proxy (`HTTP_PROXY`) is consulted
  for the render target too, and a proxy that cannot be forwarded
  (credentials, a scheme-less value) is a load error: the render never runs
  direct behind it (argv hygiene: a credential proxy is never forwarded to
  the subprocess); a typo in `fetch_render` is a loud startup error.
- SSRF: a **direct** render resolves the target through the same public-address
  rule as the plain fetch and refuses internal addresses (the render is the
  SSRF gate for its own route; `fetch_allow_private` opens it). With a proxy
  the proxy enforces the policy. Known limit: obscura follows redirects on
  its own, and a DNS-rebinding window between the pre-check and obscura's
  dial remains — run the renderer behind a proxy for policy-enforced egress.
- v1 limits: rendered docs store `final_url` = the requested URL (the dump
  mode does not report redirects); success = exit 0 + non-empty dump.
  README “JS rendering” and docs/setup.md §8.
- Docs: least-privilege store user guide for clusters with the security
  plugin (role/user/role-mapping recipes; the app role cannot delete the
  facts index; `indices:admin/get` + per-pattern create/settings/update
  actions included, with an upgrade note for roles provisioned before
  this release), linked from SECURITY.md.

Fixed:
- `memento_obsolete` (CLI and MCP) writes are scripted: the reason appends
  inside the painless script, so a concurrent atomic `notes_append` can no
  longer be dropped by the read-modify-write from the stale read (the
  contract `memento_patch` already had). A `reason` with `memento_patch` on
  an already-obsolete fact is refused (it would append a duplicate line with
  no journal entry).
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
