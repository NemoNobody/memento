#!/usr/bin/env bash
# Demo session for docs/media/quickstart.gif. Runs against a throwaway
# OpenSearch node (make dev-opensearch with MEMENTO_OS_PORT=9299) and index;
# no real data is involved. Regenerate the GIF with:
#   asciinema record -c "bash hack/quickstart-demo.sh" -f asciicast-v2 /tmp/demo.cast
#   agg --theme monokai --font-size 15 /tmp/demo.cast docs/media/quickstart.gif
set -u
export OPENSEARCH_URL=http://127.0.0.1:9299
export MEMENTO_INDEX=demo
export MEMENTO_SECRET_GUARD=allow
export MEMENTO_ALLOW_PLAINTEXT=true
export OPENSEARCH_PASSWORD=demo
export MEMENTO_AUTHOR=dev
export MEMENTO_HOST=dev-box
export PATH="$HOME/.local/bin:$PATH"
B=${MEMENTO_BIN:-memento}

run() {
  printf '\033[1;32m$\033[0m %s\n' "$*"
  sleep 0.35
  "$@"
  sleep 0.9
}

clear
sleep 0.4
run $B add demo:api:base-url "https://api.example.com/v2" config --source doc:README --keyword api
run $B add demo:api:timeout "30s" config --keyword api --ttl-days 7
run $B get demo:api:base-url
run $B add demo:api:base-url "https://api.example.com/v3" config --source doc:README --keyword api
run $B journal demo:api:base-url
run $B search v3 --keyword api
sleep 0.8
