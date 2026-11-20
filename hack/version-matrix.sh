#!/usr/bin/env bash
# OpenSearch version matrix. Brings the compose single-node dev
# stack up once per image tag, runs hack/cli-smoke.sh against it, then tears
# the whole thing down (-v: each version gets a clean infra-creation path,
# which is exactly what we want to verify). Sequential on purpose — the
# versions must not run simultaneously.
#
# Needs docker/podman access (e.g. `sg docker -c ...` where the user is not in the group).
# Port 9299 by default so a host running OpenSearch on 9200 is not clashed.
# A dedicated project name (-p) keeps this matrix off the dev stack's
# project/volume: the compose file pins `name: velruna-os`, and an unqualified
# `down -v` here would WIPE the developer's real velruna-os data volume.
#
# Usage: hack/version-matrix.sh ["2.19.7 3.0.0 ..."]   (default list below)
set -uo pipefail
cd "$(dirname "$0")/.."
VERSIONS=${*:-"2.19.7 3.0.0 3.3.2 3.8.0 3.9.0"}
PORT=${MATRIX_PORT:-9299}
BIN=${VELRUNA_BIN:-$(pwd)/velruna}
COMPOSE="docker compose -p velruna-matrix -f docker/opensearch.yml --profile single"

[ -x "$BIN" ] || { echo "binary missing: go build -o velruna . (or set VELRUNA_BIN)"; exit 1; }

CONF=$(mktemp /tmp/velruna-matrix.XXXXXX.conf)
trap 'rm -f "$CONF"' EXIT
cat >"$CONF" <<EOF
{"opensearch_url":"http://127.0.0.1:$PORT","opensearch_user":"admin","opensearch_password":"devtest","allow_plaintext":true}
EOF
chmod 600 "$CONF"

results=()
for v in $VERSIONS; do
  echo "=================== $v ==================="
  $COMPOSE down -v >/dev/null 2>&1
  if ! OPENSEARCH_IMAGE=opensearchproject/opensearch:$v VELRUNA_OS_PORT=$PORT $COMPOSE up -d; then
    results+=("$v UP-FAIL"); $COMPOSE down -v >/dev/null 2>&1; continue
  fi
  ready=""
  for _ in $(seq 1 90); do
    curl -sf --max-time 3 "http://127.0.0.1:$PORT" >/dev/null 2>&1 && { ready=1; break; }
    sleep 2
  done
  actual=$(curl -sf --max-time 3 "http://127.0.0.1:$PORT" | python3 -c 'import json,sys;print(json.load(sys.stdin)["version"]["number"])' 2>/dev/null)
  echo "endpoint reports version: ${actual:-unknown}"
  if [ -z "$ready" ]; then
    results+=("$v NOT-READY")
  elif [ "${actual#v}" != "$v" ]; then
    # The wrong image answered (a stale container, a bad tag): never score a
    # PASS against a version we did not actually bring up.
    results+=("$v WRONG-VER(got:${actual:-none})")
  elif VELRUNA_CONF="$CONF" VELRUNA_BIN="$BIN" hack/cli-smoke.sh; then
    results+=("$v PASS")
  else
    results+=("$v SMOKE-FAIL")
  fi
  $COMPOSE down -v >/dev/null 2>&1
done

echo
echo "=================== matrix results ==================="
for r in "${results[@]}"; do echo "  $r"; done
