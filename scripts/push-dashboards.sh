#!/usr/bin/env bash
# Push the memento dashboard bundle (index patterns, visualizations, the
# "Memento — store overview" dashboard) to an OpenSearch Dashboards instance.
#
# Re-runnable by design: every object carries a stable "memento-store-*" id
# and the import runs with overwrite=true, so pushing again updates in place
# instead of duplicating.
#
# Credential resolution (first hit wins):
#   1. DASHBOARDS_USER / DASHBOARDS_PASSWORD environment variables
#   2. ~/.config/memento/dashboards.env (keep it mode 600; never committed)
#   3. no auth — the dev compose recipe runs with the security plugin off
#
# Usage:
#   scripts/push-dashboards.sh                       # localhost:5601
#   DASHBOARDS_URL=http://host:5601 scripts/push-dashboards.sh
set -euo pipefail

URL="${DASHBOARDS_URL:-http://localhost:5601}"
USER="${DASHBOARDS_USER:-}"
PASS="${DASHBOARDS_PASSWORD:-}"
ENVF="${MEMENTO_DASHBOARDS_ENV:-$HOME/.config/memento/dashboards.env}"

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
  echo "set DASHBOARDS_USER/DASHBOARDS_PASSWORD or fill in $ENVF" >&2
  exit 3
fi
if [ "$code" = "000" ]; then
  echo "dashboards unreachable: $URL" >&2
  exit 1
fi

resp=$(curl -s --max-time 30 -X POST "$URL/api/saved_objects/_import?overwrite=true" "${AUTH[@]}" -F "file=@$OBJ")
if printf '%s' "$resp" | grep -q '"success":true'; then
  echo "imported to $URL: dashboard \"Memento — store overview\", index patterns memento / memento-history-* / memento-docs"
else
  echo "import failed: $resp" >&2
  exit 4
fi
