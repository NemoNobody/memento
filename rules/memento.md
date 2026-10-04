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
- Value changed → correct via the same key; the old value moves to history. A key names a stable subject, not a value (`proj:api:port`, not `proj:api:v1`).
- Qualifiers and doubts → `notes` ("checked the dev config only"). Subject gone → `memento_obsolete <key> [reason]`.
- **Never store secrets** (passwords, tokens, keys) — the store is plaintext with no filter; the rule is on you.
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
| `memento_add <key> <value> [category] [project] [source] [notes]` | upsert |
| `memento_obsolete <key> [reason]` | the subject is gone |

### When to search, when to store

- **Search before re-verifying**: file locations, endpoints, config values,
  behavior, versions.
- **Store after verifying**, if the fact will be useful to another session.
- **Do not store**: the obvious, the one-off, secrets.

### The key is the identity

- A key names a stable subject or role, not its current value.
- Format `project:area:detail`: `demo:api:port`, `myrepo:config:opensearch_url`.
- "Same fact or a different one" is decided by the key. Value changed — same
  key, a correction; subject changed — a different key.

### Statuses, history, resurrection

- `verified` — the subject exists; `obsolete` — the subject is gone (file
  deleted, endpoint removed without a successor).
- **Value changed** → `memento_add` with the same key: the old value moves to
  `history`, `observed_at` = now. That is a correction, not a loss.
- **Same key + same value** → no history entry; only `notes`/`source` are
  updated (and `observed_at` stays — the observation is old, the note is new).
- **Subject gone** → `memento_obsolete <key> [reason]`: value and `observed_at`
  are kept, the reason is appended to `notes`.
- **Subject back** → `memento_add` resurrects it: `obsolete` → `verified`, the
  old value moves to history.
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

The store provides evidence (`source`, `observed_at`, `history`); it does not
decide. "Old" depends on context: two years is fine for a port; a week is an
eternity for a token.

### Security

- The store is **plaintext** and nothing filters what is written: never store
  passwords, tokens, keys, or customer data. This rule is on you, not on the
  store.
- Do not echo secrets in the conversation.

### Degradation (store unreachable)

- `memento_status` → `ok=false` plus a hint; tell the user explicitly.
- An empty answer is not "no such fact": the store tools fail loudly instead of
  staying silent.
- Do not retry in a loop.

### Session start

`memento_list <project>` — always, before the first verification about the
project.
