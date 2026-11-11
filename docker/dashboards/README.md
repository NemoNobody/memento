# Dashboards bundle

`objects.ndjson` — saved objects for the "Memento — store overview"
dashboard: 3 index patterns, 13 visualizations, the dashboard itself.
Stable `memento-store-*` ids make the push idempotent (update in place).

Push with `scripts/push-dashboards.sh` (wallet-first credentials, see its
header). The script talks to the CLUSTER, not the Dashboards port: it indexes
the objects directly into the `.kibana` alias backing index, which IS the
global tenant's storage. The saved-objects REST API is not used on purpose —
the `security_tenant: global` header is ignored for basic-auth requests
(verified on OSD 3.8): REST writes always land in the authenticated user's
personal tenant index (`.kibana_<hash>_<user>_1`) and never appear in the
global UI. Direct indexing writes the canonical OSD 3.x typed shape: doc id
`<type>:<id>`, attributes nested under a `"<type>"` field, pinned
`migrationVersion` (index-pattern 7.6.0, visualization 7.10.0,
dashboard 7.9.3). After a push, switch the UI to the Global tenant and hard
reload (the SPA caches the index-pattern list per session).
Do NOT use the OSD `_import` endpoint: OSD 3.x strips the
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
- the dashboard's `panelsJSON` must be a JSON **array** of panel entries
  (`{"version","type":"visualization","panelIndex":N,"panelRefName":
  "panel_N","gridData":{"x","y","w","h","i"},"embeddableConfig":{}}`);
  the map form `{"p1":{...}}` saves without complaint but the dashboard
  renders completely empty;
- the classic grid is **12 columns** wide: keep `x+w<=12`, overflowing
  panels stack/squash near the origin;
- patterns must list the `_index` meta field explicitly (aggregatable
  "string", esType `_index`) for aggs on it (gen_fields.py META_FIELDS);
- `visState` aggs live at the TOP level (`{"title","type","params",
  "aggs":[...]}`), not under `params`;
- the dashboard pins `timeRestore` + `now-90d..now`, otherwise the
  default "last 15 minutes" renders it empty.

Panel inventory: 4 metric KPIs (facts, journal entries, docs cached,
facts with history), 3 time-area charts (facts updates / journal writes /
docs fetched over time), journal ops histogram, journal entries per period
index, three donuts (project/category/status), top fact keys horizontal bar.
Index SIZES are deliberately absent: sizes live outside searchable data and
Vega url data sources are GET-only (the OSD console proxy is POST-only).
Sizes trend = roadmap item 30 (memento metrics). Live sizes meanwhile:
Stack Management -> Index Management, or `memento status` for the cluster view.

After a mapping change (new fact/journal/doc fields) regenerate the field
lists from a live cluster and push again:

```
python3 docker/dashboards/gen_fields.py   # reads memento.conf + wallet
scripts/push-dashboards.sh
```
