# Dashboard

A ready-made OpenSearch Dashboards dashboard over the facts store — import it
and the state of memory is visible at a glance:

| Panel | Answers |
|-------|---------|
| Total facts | how much is stored |
| By status / project / category / source | what kind of knowledge is in the store |
| Observation activity | when facts were observed (histogram over `observed_at`) |
| Oldest observations (re-check first) | the re-verification queue: facts that age the longest |
| Facts with notes (qualifiers) | facts carrying caveats (`notes:*`) |

## Import

`dashboards/memento-dashboard.ndjson` holds 10 saved objects: the `memento`
data view (index pattern with time field `observed_at`), 8 visualizations and
the dashboard itself.

**UI:** Dashboards Management → Saved Objects → Import → select the file.
The dashboard opens as `memento — facts memory`.

**API:**

```bash
curl -u admin:<your-password> -H 'osd-xsrf: true' \
  -F file=@dashboards/memento-dashboard.ndjson \
  'http://localhost:5601/api/saved_objects/_import?overwrite=true'
```

Two things to know:

- **Tenants.** Saved objects live per tenant, and the import lands in the
  tenant you are in (`Private` by default). For the shared `Global` tenant add
  `-H 'securitytenant: global'`. Switch the view via avatar menu → Switch
  tenants.
- **Field list.** The data view carries its own copy of the field list; it is
  shipped inside the NDJSON, so panels resolve fields right after import.
  After a mapping change, refresh it: Dashboards Management → Index patterns →
  the `memento` data view → Refresh field list.

## How it reads facts

- The time field is `observed_at`, and the dashboard applies a time range
  (`now-10y`…`now`). **Facts without `observed_at` do not appear** — a
  time-filtered query skips documents that lack the time field. Facts written
  by `memento` always carry it; older ones can be backfilled (below).
- The journal history of accepted values lives in journal indices and is not
  modeled in Dashboards; read it with `memento journal <key>` (entries) or
  `memento history-info <key>` (counts). The legacy in-doc `history` array
  (pre-journal facts) is stored but not indexed, so it never appears in
  Dashboards — read it with `memento get <key> --history`.

## Backfilling `observed_at`

Facts created before the field existed have none. Backfill from `updated_at` —
the same fallback `memento` uses when a fact's value changes:

```bash
curl -k -u admin:<your-password> -H 'Content-Type: application/json' \
  'https://127.0.0.1:9200/memento/_update_by_query?refresh=true&conflicts=proceed' -d '{
    "script": {
      "lang": "painless",
      "source": "if (ctx._source.observed_at == null) { ctx._source.observed_at = ctx._source.updated_at != null ? ctx._source.updated_at : ctx._source.created_at; }"
    },
    "query": {"bool": {"must_not": [{"exists": {"field": "observed_at"}}]}}
  }'
```

## Troubleshooting

| Symptom | Cause / fix |
|---------|-------------|
| `Could not locate that index-pattern-field (id: …)` on panels | The data view has no field list — re-import the NDJSON (it includes the fields), or Refresh field list |
| Dashboard renders but shows no facts | The facts lack `observed_at` — backfill (above) |
| Dashboard not in the list | It was imported into another tenant — avatar → Switch tenants and look there |
