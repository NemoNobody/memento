# memento — agent rule

An importable rule for AI agents: how to use **memento** as persistent,
provenance-carrying memory. Keep it where your agent reads its rules:

- `AGENTS.md` / `.github/copilot-instructions.md` (repo-level)
- `.cursor/rules/`, `.claude/CLAUDE.md`
- VS Code user instructions (`*.instructions.md`)
- any always-on system prompt

## Compact version

Paste this into your always-on rules:

```
- Start of a session in a project → `memento_list <project>` (always).
- Before re-verifying something → `memento_search <query>` / `memento_get <key>`; after verifying something new → `memento_add`.
- Value changed → correct via the same key; the accepted value is appended to the history journal. A key names a stable subject — predictable English, not a value (`proj:api:port`, not `proj:api:v1`).
- Qualifiers and doubts → `notes` ("checked the dev config only"). Subject gone → `memento_obsolete <key> [reason]`.
- Reads return the current value only (`stale`/`age_days` included); history costs context — use `memento_history_info` (cheap counts) and `memento_journal` (the entries) only when the history matters. `stale: true` → re-verify against the source before trusting.
- `memento_prune` is destructive and manual (the journal is write-forever by design): without `confirm: true` it only returns the plan; show the plan to the user and confirm only after their approval.
- **Never store secrets** (passwords, tokens, keys) — the store is plaintext;
  a best-effort secret guard (default `reject`) scans writes, but it is not
  protection: the rule is on you.
- Store unreachable → say so (via `memento_status`); never silently re-verify and never retry in a loop.
```

## Full version

`memento` is **memory**; the LLM is **reasoning**. The store holds evidence —
value, source, observation time, history, notes — and the agent judges trust and
staleness in context. The store never decides.

### Tools

| Tool | Purpose |
|---|---|
| `memento_status` | is the store reachable; never fails — `ok=false` plus a hint |
| `memento_list <project>` | all facts of a project — session start |
| `memento_search <query>` | search in `value` and `notes` (not in keys) |
| `memento_get <key>` | exact key (`found=false` when absent) |
| `memento_add <key> <value> [category] [project] [source] [notes]` | upsert (categories: location, config, endpoint, behavior, context_map — unknown ones are rejected) |
| `memento_history_info <key>` | cheap history overview: writes, rediscoveries, first/last — no entry bodies |
| `memento_journal <key> [limit]` | the journal: every accepted value, newest first (read it only when the history matters) |
| `memento_prune <key> [keep]` | drop old journal entries (destructive: plan first, `confirm=true` only after the user approves) |
| `memento_obsolete <key> [reason]` | the subject is gone |

### When to search, when to store

- **Search before re-verifying**: file locations, endpoints, config values,
  behavior, versions.
- **Store after verifying**, if the fact will be useful to another session.
- **Do not store**: the obvious, the one-off, secrets.

### The key is the identity

- A key names a stable subject or role, not its current value.
- Keys are plain **English and predictable** — the next session must be able to
  guess the key without searching. Format `project:area:detail`:
  `demo:api:port`, `myrepo:config:opensearch_url`.
- "Same fact or a different one" is decided by the key. Value changed — same
  key, a correction; subject changed — a different key.

### Statuses, history, resurrection

- `verified` — the subject exists; `obsolete` — the subject is gone (file
  deleted, endpoint removed without a successor).
- **Value changed** → `memento_add` with the same key: the accepted value is
  journaled and the fact points at it, `observed_at` = now. That is a
  correction, not a loss.
- **Same key + same value** → no journal entry; only `notes`/`source` are
  updated (and `observed_at` stays — the observation is old, the note is new).
- **Subject gone** → `memento_obsolete <key> [reason]`: value and `observed_at`
  are kept, the reason is appended to `notes`.
- **Subject back** → `memento_add` resurrects it: `obsolete` → `verified`, the
  new observation is journaled.
- `obsolete` ≠ "the fact became false": "false" usually means the value
  changed — that is a correction.

### source (provenance)

A label of where it came from — not a measure of trust:

- `tool:read_file` — read in code/config
- `experiment` — verified by running something
- `user:confirmed` — the user confirmed it
- `doc:*` — documentation (**a pointer, not the truth**: docs drift)
- `agent` — the agent's own conclusion

If a value matters, verify against the code; `doc:*` is only "where to look".

### notes — a bridge between sessions

`notes` holds qualifiers that are not facts: "checked the dev config only",
"conflicts with key X".

**Precision costs resources.** If you only checked part of it, record *how far*
you got — otherwise the next session will either trust an under-verified fact or
re-verify everything from scratch. A doubt is a note, not a status.

### Staleness is the LLM's call

The store provides evidence (`source`, `observed_at`, the journal) **and a
staleness signal** (`age_days`, `stale`, computed at read time from a
per-category TTL: `config`/`endpoint` 14 days, `location`/`behavior` 30 days,
`context_map` and unknown categories never). It does not decide. "Old" depends
on context: two years is fine for a port; a week is an eternity for a token.
`stale: true` is an instruction to re-verify against the source, not to
discard. An unknown `observed_at` (`age_days: -1`) counts as stale for TTL'd
categories — you cannot tell how old it is.

### History is off by default

Reads (`memento_get`/`memento_search`/`memento_list`) return the current value
only. The history of accepted values lives in the append-only journal: use
`memento_history_info` first (writes, rediscoveries, the time range — cheap),
and `memento_journal` when the entries themselves are needed. `include_history`
returns the legacy in-doc array of pre-journal facts. The journal is
write-forever; `memento_prune` trims one key on demand and is destructive: the
plan comes back without `confirm: true`; write only after the user approves.

### Security

- The store is **plaintext**. A best-effort secret guard (default mode
  `reject`) scans what is written, but it can be bypassed by design: never
  store passwords, tokens, keys, or customer data. This rule is on you, not
  on the store.
- Do not echo secrets in the conversation.

### Degradation (store unreachable)

- `memento_status` → `ok=false` plus a hint; tell the user explicitly.
- An empty answer is not "no such fact": the store tools fail loudly instead of
  staying silent.
- A page reports `truncated` (the cluster saw more matches than the cap —
  narrow the query) and `skipped` (stored content failed to decode — index
  corruption: report to the user, never treat the answer as complete).
- Do not retry in a loop.

### Session start

`memento_list <project>` — always, before the first verification about the
project.
