# Dashboard

A ready-made OpenSearch Dashboards overview of the store — "Velruna — store
overview". It covers facts, the journal (history) and cached documents:

| Panel | Answers |
|-------|---------|
| Facts / Journal entries / Docs cached / Facts with history (KPI row) | how much is stored, how live the journal is |
| Facts updates over time / Journal writes over time / Docs fetched over time | activity trends (area charts over the time field) |
| Journal ops | writes/patches/rediscoveries per bucket (histogram over `op`) |
| Journal entries per period index | how the monthly history indices grow |
| By project / category / status (donuts) | what kind of knowledge is in the store |
| Top fact keys by journal writes | the hottest facts (re-verification candidates) |
| Docs by via / fetched_by / project (donuts) | which route fetched the cache (plain, proxy, renderer), which host, for which project |
| Docs past TTL | snapshots whose `expires_at` has passed: re-fetch candidates (the cached content is still readable) |

## Install

The bundle (`docker/dashboards/objects.ndjson`: 3 index patterns, 18
visualizations, the dashboard) is pushed with the bundled script:

```bash
scripts/push-dashboards.sh
```

On the dev stack `make dev-opensearch-dashboards` does both at once — it
starts the UI, waits for it and pushes the bundle automatically.

It resolves cluster credentials the same way the server does (wallet first)
and writes the saved objects **directly into the global tenant storage** (the
`.kibana` alias backing index). Do not use the Dashboards UI `_import`: OSD
3.x import strips each visualization's index reference and the panels then
die with "Trying to initialize aggs without index pattern". The
`securitytenant`/`security_tenant` HTTP header is ignored for basic-auth
requests — a REST push always lands in the pushing user's personal tenant,
which is why the script indexes the objects itself.

After the push: open Dashboards, make sure the tenant switcher (avatar menu →
Switch tenants) shows **Global**, and hard-reload the tab — the SPA caches
the index-pattern list per session. A quick start for the whole stack:
`make dev-opensearch && make dev-opensearch-dashboards` (see
`docker/dashboards/README.md` for the bundle's invariants).

## How it reads the store

> An older store that still uses the pre-rename index names
> (`index_name`/`docs_index_name` pinned to `memento*`) needs the index
> patterns edited in the bundle before pushing, and the `memento-store-*`
> saved objects from an earlier push removed.

- Three index patterns with time fields: `velruna` (facts, `updated_at`),
  `velruna-history-*` (journal, `created_at`), `velruna-docs` (doc snapshots,
  `fetched_at`). The dashboard pins a `now-90d` window (`timeRestore`).
- The index patterns carry their field lists inside the bundle (OSD cannot
  introspect them over the API). After a mapping change regenerate and push:

  ```bash
  python3 docker/dashboards/gen_fields.py
  scripts/push-dashboards.sh
  ```

- Journal entries are also readable without Dashboards: `velruna journal
  <key>` (entries) and `velruna_history_info <key>` (counts).

## Troubleshooting

| Symptom | Cause / fix |
|---------|-------------|
| `Trying to initialize aggs without index pattern` | the objects came through `_import` — push with `scripts/push-dashboards.sh` instead |
| `Could not locate that index-pattern-field (id: …)` | stale/empty field list in a pattern — re-run `gen_fields.py` + push |
| Dashboard opens completely empty (no panels) | hand-edited `panelsJSON` in map form — it must be a JSON array (the shipped bundle is correct) |
| `AnalyticEngineError: does not support DSL queries` | stale SPA cache — hard reload the tab |
| Dashboard not in the list | you are in another tenant — avatar → Switch tenants → Global |
