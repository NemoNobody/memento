# Changelog

Notable changes per release tag. v0.6.0 is the pre-publication hardening
release; v0.1.0 was the first internal build.

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
