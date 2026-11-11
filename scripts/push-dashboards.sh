#!/usr/bin/env bash
# Push the memento dashboard bundle (index patterns, visualizations, the
# "Memento — store overview" dashboard) to an OpenSearch Dashboards instance.
#
# Re-runnable by design: every object carries a stable "memento-store-*" id
# and each object is created (POST) or updated (PUT) directly through the
# saved-objects API, in place. The _import endpoint is deliberately NOT used:
# OSD 3.x import strips the index reference out of searchSourceJSON and the
# visualization then fails with "Trying to initialize aggs without index
# pattern". The per-object API stores the canonical UI shape (placeholder +
# references) verbatim.
#
# Credential resolution (first hit wins):
#   1. DASHBOARDS_USER / DASHBOARDS_PASSWORD environment variables
#   2. `memento creds get` — the wallet/keyring entry of the configured
#      cluster (same entry the MCP server itself uses; point at a config
#      outside the cwd with MEMENTO_CONF=/path/to/memento.conf)
#   3. ~/.config/memento/dashboards.env — fallback for headless boxes with
#      no session bus (no wallet reachable)
#   4. no auth — the dev compose recipe runs with the security plugin off
#
# Usage:
#   scripts/push-dashboards.sh                       # localhost:5601
#   DASHBOARDS_URL=http://host:5601 scripts/push-dashboards.sh
set -euo pipefail

URL="${DASHBOARDS_URL:-http://localhost:5601}"
USER="${DASHBOARDS_USER:-}"
PASS="${DASHBOARDS_PASSWORD:-}"
ENVF="${MEMENTO_DASHBOARDS_ENV:-$HOME/.config/memento/dashboards.env}"

if [ -z "$PASS" ]; then
  MEM=$(command -v memento || true)
  [ -z "$MEM" ] && [ -x "$HOME/.local/bin/memento" ] && MEM="$HOME/.local/bin/memento"
  if [ -n "$MEM" ]; then
    # exit status matters: an old memento without `creds get` prints usage
    # to stdout, and that text must never pass as a password
    if TRY=$([ -n "$MEM" ] && "$MEM" creds get 2>/dev/null); then
      PASS=$TRY
    fi
  fi
fi
if [ -z "$PASS" ] && [ -f "$ENVF" ]; then
  set -a
  # shellcheck disable=SC1090
  . "$ENVF"
  set +a
  USER="${DASHBOARDS_USER:-$USER}"
  PASS="${DASHBOARDS_PASSWORD:-$PASS}"
fi

here=$(cd "$(dirname "$0")" && pwd)
OBJ="$here/../docker/dashboards/objects.ndjson"
if [ ! -f "$OBJ" ]; then
  echo "objects bundle not found: $OBJ" >&2
  exit 2
fi

AUTH=()
[ -n "$PASS" ] && AUTH=(-u "${USER:-admin}:$PASS")

code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "$URL/api/status" "${AUTH[@]}" || echo 000)
if [ "$code" = "401" ] && [ -z "$PASS" ]; then
  echo "dashboards requires authentication ($URL answered 401)." >&2
  echo "give the wallet an entry ('memento creds set'), set DASHBOARDS_USER/DASHBOARDS_PASSWORD, or fill in $ENVF" >&2
  exit 3
fi
if [ "$code" = "000" ]; then
  echo "dashboards unreachable: $URL" >&2
  exit 1
fi

# Per-object create/update. Credentials travel through the environment: the
# password never enters argv (ps shows command lines).
DASH_URL="$URL" DASH_USER="${USER:-admin}" DASH_PASS="$PASS" DASH_OBJ="$OBJ" \
python3 - <<'PY'
import json, os, sys, urllib.request, urllib.error

url = os.environ["DASH_URL"].rstrip("/")
user = os.environ.get("DASH_USER") or "admin"
pw = os.environ.get("DASH_PASS") or ""
import base64
auth = base64.b64encode(f"{user}:{pw}".encode()).decode() if pw else None

def req(method, path, body=None):
    r = urllib.request.Request(url + path, method=method)
    r.add_header("osd-xsrf", "true")
    r.add_header("Content-Type", "application/json")
    if auth:
        r.add_header("Authorization", "Basic " + auth)
    if body is not None:
        r.data = json.dumps(body).encode()
    return r

ok = 0
failed = 0
for line in open(os.environ["DASH_OBJ"], encoding="utf-8"):
    o = json.loads(line)
    path = f"/api/saved_objects/{o['type']}/{o['id']}"
    body = {"attributes": o["attributes"], "references": o.get("references", [])}
    try:
        with urllib.request.urlopen(req("PUT", path, body), timeout=30):
            ok += 1
    except urllib.error.HTTPError as e:
        if e.code == 404:  # first push: create with the explicit id
            try:
                with urllib.request.urlopen(req("POST", path, body), timeout=30):
                    ok += 1
            except urllib.error.HTTPError as e2:
                failed += 1
                print(f"create {o['type']}/{o['id']}: {e2.code} {e2.read().decode()[:200]}", file=sys.stderr)
        else:
            failed += 1
            print(f"update {o['type']}/{o['id']}: {e.code} {e.read().decode()[:200]}", file=sys.stderr)

# Index patterns carry their fields baked in (see the note in
# docker/dashboards/README.md): OSD cannot resolve a pattern's timeFieldName
# while fields is empty, and every OSD route that could introspect fields
# (index_patterns/data_views/proxy) is UI-internal or absent in 3.x. To
# refresh after a mapping change: regenerate the bundle from a live cluster.
print(f"pushed {ok} saved object(s) to {url}: index patterns memento / memento-history-* / memento-docs, dashboard \"Memento — store overview\"")
sys.exit(1 if failed else 0)
PY
