# Dashboards bundle

`objects.ndjson` — saved objects for the "Memento — store overview"
dashboard: 3 index patterns, 8 visualizations, the dashboard itself.
Stable `memento-store-*` ids make the push idempotent (update in place).

Push with `scripts/push-dashboards.sh` (wallet-first credentials, see its
header). Do NOT use the OSD `_import` endpoint: OSD 3.x strips the
visualization's index reference during import and the panels then fail
with "Trying to initialize aggs without index pattern".

Registered classic visualization types in OSD 3.8 (from the source):
pie/line/area/histogram/horizontal_bar/heatmap/gauge/goal (vislib),
metric (vis_type_metric), table, markdown, tagcloud, vega, timelion,
metrics (TSVB — needs panel config, do not hand-write), tile_map,
region_map, input_control_vis. `basic` was REMOVED in 3.x.
`params: {}` is valid — vislib merges its defaults.

Bundle invariants (OSD 3.x quirks, all learned live):

- visualizations reference their index pattern via
  `"indexRefName": "kibanaSavedObjectMeta.searchSourceJSON.index"` inside
  `searchSourceJSON` plus a matching `references[]` entry — the old
  `"index": "<name>"` placeholder form is treated as a literal id and
  fails with "The index pattern ... no longer exists";
- index patterns must carry a non-empty `fields` list up front: an empty
  list cannot resolve the `timeFieldName` and every query dies with
  "Could not locate that index-pattern-field";
- index-pattern field descriptors use the OSD field-type enum, not the raw
  es type: a saved field of type "keyword" fails Terms aggs ("is invalid
  for use with the Terms aggregation") — keyword/text map to "string",
  long/integer to "number" (gen_fields.py TYPE_MAP encodes this);
- visualization `attributes` must not contain `optionsJSON` (strict
  saved-object mapping rejects it; options live inside `visState`);
- the dashboard pins `timeRestore` + `now-90d..now`, otherwise the
  default "last 15 minutes" renders it empty.

After a mapping change (new fact/journal/doc fields) regenerate the field
lists from a live cluster and push again:

```
python3 docker/dashboards/gen_fields.py   # reads memento.conf + wallet
scripts/push-dashboards.sh
```
