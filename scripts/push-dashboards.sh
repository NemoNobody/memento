#!/usr/bin/env bash
# Push the memento dashboard bundle (index patterns, visualizations, the
# "Memento — store overview" dashboard) into an OpenSearch cluster's GLOBAL
# Dashboards tenant by indexing the saved objects straight into the .kibana
# alias backing index.
#
# Why not the saved-objects REST API: the "security_tenant: global" header is
# IGNORED for basic-auth requests (verified on OSD 3.8) — the tenant always
# follows the browser auth cookie, so a scripted push can never target the
# global tenant through the API; it silently lands in the user's personal
# tenant index (.kibana_<hash>_<user>_1). Direct indexing into the .kibana
# alias write index is the canonical global-tenant storage: every saved
# object is a doc with _id "<type>:<id>" in the OSD 3.x typed shape
# (attributes nested under a "<type>" field, migrationVersion pinned).
#
# Re-runnable by design: every object carries a stable "memento-store-*" id
# and each doc is PUT in place. The _import endpoint is deliberately NOT
# used: OSD 3.x import strips the index reference out of searchSourceJSON
# and the visualization then fails with "Trying to initialize aggs without
# index pattern".
#
# This talks to the CLUSTER (default https://127.0.0.1:9200), not to the
# Dashboards web port. Credentials (first hit wins, same chain as
# docker/dashboards/gen_fields.py):
#   1. MEMENTO_OS_URL / MEMENTO_OS_USER / OPENSEARCH_PASSWORD env vars
#   2. memento.conf (opensearch_url / opensearch_user) + `memento creds get`
#      — the wallet entry of the configured cluster (MEMENTO_CONF=... to
#      point at a config outside the cwd)
#   3. ~/.config/memento/dashboards.env — fallback for headless boxes with
#      no session bus (no wallet reachable)
#   4. no auth — the dev compose recipe runs with the security plugin off
#
# Usage:
#   scripts/push-dashboards.sh
#   MEMENTO_OS_URL=https://host:9200 scripts/push-dashboards.sh
#   MEMENTO_KIBANA_INDEX=.kibana_1 scripts/push-dashboards.sh  # skip alias resolution
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
OBJ="$here/../docker/dashboards/objects.ndjson"
if [ ! -f "$OBJ" ]; then
  echo "objects bundle not found: $OBJ" >&2
  exit 2
fi

ENVF="${MEMENTO_DASHBOARDS_ENV:-$HOME/.config/memento/dashboards.env}"
if [ -f "$ENVF" ]; then
  set -a
  # shellcheck disable=SC1090
  . "$ENVF"
  set +a
fi

# Credentials travel through the environment: the password never enters argv.
MEMENTO_OBJ="$OBJ" python3 - <<'PY'
import base64
import json
import os
import ssl
import subprocess
import sys
import urllib.parse
import urllib.request
from datetime import datetime, timezone

# OSD 3.x per-type migrationVersion as written by the UI itself; the bundle
# only carries these three types, unknown types are rejected loudly.
MIGRATION = {
    "index-pattern": "7.6.0",
    "visualization": "7.10.0",
    "dashboard": "7.9.3",
}

conf_path = os.environ.get("MEMENTO_CONF", "memento.conf")
conf = {}
if os.path.exists(conf_path):
    conf = json.load(open(conf_path, encoding="utf-8"))

url = os.environ.get("MEMENTO_OS_URL") or conf.get("opensearch_url") or "https://127.0.0.1:9200"
user = os.environ.get("MEMENTO_OS_USER") or conf.get("opensearch_user", "admin")
pw = os.environ.get("OPENSEARCH_PASSWORD") or ""
if not pw:
    memento = os.environ.get("MEMENTO_BIN") or "memento"
    # exit status matters: an old memento without `creds get` prints usage
    # to stdout, and that text must never pass as a password
    r = subprocess.run([memento, "creds", "get"], capture_output=True, text=True)
    if r.returncode == 0:
        pw = r.stdout.strip()
    else:
        pw = ""
if not pw and os.environ.get("DASHBOARDS_PASSWORD"):
    pw = os.environ["DASHBOARDS_PASSWORD"]
    user = os.environ.get("DASHBOARDS_USER") or user

auth = base64.b64encode(f"{user}:{pw}".encode()).decode() if pw else None
ctx = ssl._create_unverified_context()


def req(method, path, body=None):
    r = urllib.request.Request(url.rstrip("/") + path, method=method)
    r.add_header("Content-Type", "application/json")
    if auth:
        r.add_header("Authorization", "Basic " + auth)
    if body is not None:
        r.data = json.dumps(body).encode()
    return r


def call(method, path, body=None):
    with urllib.request.urlopen(req(method, path, body), context=ctx, timeout=30) as resp:
        raw = resp.read().decode()
    return json.loads(raw) if raw else {}


# The global tenant's saved objects live in the .kibana alias write index.
index = os.environ.get("MEMENTO_KIBANA_INDEX")
if not index:
    try:
        rows = call("GET", "/_cat/aliases/.kibana?format=json")
    except Exception as e:  # noqa: BLE001
        sys.exit(f"cannot resolve the .kibana alias at {url}: {e}\n"
                 "is OpenSearch up and is Dashboards initialized? "
                 "(override with MEMENTO_KIBANA_INDEX=.kibana_1)")
    write = [r["index"] for r in rows if r.get("write_index") in ("true", True)]
    index = write[0] if write else (rows[0]["index"] if rows else None)
if not index:
    sys.exit("the .kibana alias resolves to no index — has Dashboards ever started against this cluster?")

objs = [json.loads(line) for line in open(os.environ["MEMENTO_OBJ"], encoding="utf-8")]
now = datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z")

ok = 0
failed = 0
for o in objs:
    t = o["type"]
    if t not in MIGRATION:
        failed += 1
        print(f"unsupported type {t!r} for {o['id']}: extend MIGRATION map", file=sys.stderr)
        continue
    doc = {
        "type": t,
        "updated_at": now,
        "migrationVersion": {t: MIGRATION[t]},
        "references": o.get("references", []),
        t: o["attributes"],
    }
    qid = urllib.parse.quote(f"{t}:{o['id']}")
    try:
        call("PUT", f"/{index}/_doc/{qid}?refresh=true", doc)
        ok += 1
    except Exception as e:  # noqa: BLE001
        failed += 1
        print(f"index {t}:{o['id']}: {e}", file=sys.stderr)

call("POST", f"/{index}/_refresh")
print(f"indexed {ok} saved object(s) into {index} (global tenant) at {url}: "
      "index patterns memento / memento-history-* / memento-docs, "
      'dashboard "Memento — store overview"')
print("Dashboards: pick the Global tenant in the tenant switcher and hard-reload the tab "
      "(the SPA caches the index-pattern list per session).")
sys.exit(1 if failed else 0)
PY
