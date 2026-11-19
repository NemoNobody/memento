#!/usr/bin/env bash
# memento installer. Downloads the binaries published by
# release.yml (memento-<os>-<arch> + checksums.txt), verifies sha256 and
# installs into ~/.local/bin (override: --install-dir / MEMENTO_INSTALL_DIR).
#
#   scripts/install.sh [--version v0.8.3] [--install-dir DIR]
#
# Default version = the latest GitHub release. Installing from a private fork
# or mirror needs GITHUB_TOKEN (a fine-grained PAT with contents:read); public
# installs need no token. BASE_URL exists for tests (mirrors the release
# download layout).
# Windows: download memento-windows-amd64.exe from the release page and place
# it on PATH — this script covers linux/darwin.
set -euo pipefail

REPO="NemoNobody/memento"
API="https://api.github.com"
BASE_URL="${BASE_URL:-https://github.com/$REPO}"
VERSION=""
INSTALL_DIR="${MEMENTO_INSTALL_DIR:-$HOME/.local/bin}"

usage() { sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'; exit 0; }

while [ $# -gt 0 ]; do
  case "$1" in
    --version)
      [ $# -ge 2 ] || { echo "--version needs a value (e.g. --version v0.8.3)" >&2; exit 1; }
      VERSION=$2; shift 2 ;;
    --install-dir)
      [ $# -ge 2 ] || { echo "--install-dir needs a path" >&2; exit 1; }
      INSTALL_DIR=$2; shift 2 ;;
    --help|-h)     usage ;;
    *) echo "unknown argument: $1 (--help)" >&2; exit 1 ;;
  esac
done

os=$(uname -s | tr 'A-Z' 'a-z')
case "$os" in
  linux|darwin) ;;
  *) echo "unsupported OS: $os (on Windows use the .exe from the release page)" >&2; exit 1 ;;
esac
arch=$(uname -m)
case "$arch" in
  x86_64|amd64)  arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "unsupported arch: $arch" >&2; exit 1 ;;
esac

auth=()
[ -n "${GITHUB_TOKEN:-}" ] && auth=(-H "Authorization: Bearer $GITHUB_TOKEN")
# Bounded, retried curl. The auth array is empty on public installs, and an
# empty "${auth[@]}" trips `set -u` on bash <4.4 (stock macOS 3.2), hence the
# +expansion guard folded in once here so every call site stays simple.
curlargs=(-fsSL --retry 3 --retry-delay 1 --max-time 120 ${auth[@]+"${auth[@]}"})

if [ -z "$VERSION" ]; then
  command -v python3 >/dev/null || { echo "need python3 to parse the latest release (or pass --version)" >&2; exit 1; }
  if ! latest=$(curl "${curlargs[@]}" "$API/repos/$REPO/releases/latest" 2>/dev/null); then
    echo "could not read the latest release from $API/repos/$REPO/releases/latest" >&2
    echo "  a private repo needs GITHUB_TOKEN (fine-grained PAT, contents:read); or pin one: --version v0.8.3" >&2
    exit 1
  fi
  VERSION=$(printf '%s' "$latest" | python3 -c 'import json,sys;print(json.load(sys.stdin)["tag_name"])')
fi

ASSET="memento-$os-$arch"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "downloading $ASSET $VERSION ..."
curl "${curlargs[@]}" -o "$tmp/$ASSET"        "$BASE_URL/releases/download/$VERSION/$ASSET"
curl "${curlargs[@]}" -o "$tmp/checksums.txt" "$BASE_URL/releases/download/$VERSION/checksums.txt"

# Verify sha256 against the release checksums. The second field is compared
# exactly (memento-linux-amd64 must not match memento-linux-arm64's line).
want=$(awk -v a="$ASSET" '$2==a{print $1}' "$tmp/checksums.txt")
if [ -z "$want" ]; then echo "no checksum line for $ASSET in checksums.txt" >&2; exit 1; fi
if command -v sha256sum >/dev/null; then got=$(sha256sum "$tmp/$ASSET" | awk '{print $1}');
else got=$(shasum -a 256 "$tmp/$ASSET" | awk '{print $1}'); fi
if [ "$want" != "$got" ]; then echo "CHECKSUM MISMATCH: want $want got $got" >&2; exit 1; fi
echo "checksum verified: $got"

chmod +x "$tmp/$ASSET"
mkdir -p "$INSTALL_DIR"
mv "$tmp/$ASSET" "$INSTALL_DIR/memento"
echo "installed: $INSTALL_DIR/memento"
"$INSTALL_DIR/memento" --version || true

case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) echo; echo "NOTE: $INSTALL_DIR is not on PATH — add it to your shell profile." ;;
esac

cat <<'SNIPPET'

Next steps (see docs/setup.md for the full walkthrough):
  1. Create a config — the default lookup finds ~/.memento.conf:
       {
         "opensearch_url": "https://127.0.0.1:9200",
         "opensearch_user": "admin",
         "insecure_skip_verify": true
       }
     Store the password in the OS keyring:  memento creds set
  2. Sanity check:  memento status
  3. Register the MCP server in your editor (docs/mcp.md).
SNIPPET
