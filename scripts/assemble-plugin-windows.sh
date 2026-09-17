#!/usr/bin/env bash
#
# assemble-plugin-windows.sh assembles one Windows-only Claude Code plugin
# tree from the repo's static plugin-windows/ files plus a single freshly
# cross-compiled ghost.exe, and stamps the release version and the
# arch-qualified plugin name into the manifest.
#
# Why one archive per Windows architecture: Claude Code serves every host
# from one .mcp.json and one hooks/hooks.json command, and there is no
# per-OS command (anthropics/claude-code#15562 is closed as not planned).
# A native .exe cannot serve both windows-amd64 and windows-arm64, and a
# required userConfig arch option reproduces the #470 failure (install
# succeeds but the option is unset and the placeholder path never runs), so
# the marketplace lists one zero-config entry per architecture instead:
# ghost-windows-amd64 and ghost-windows-arm64. The POSIX plugin stays
# zero-config through bin/ghost-launcher and is assembled by the sibling
# scripts/assemble-plugin.sh. See .claude-plugin/marketplace.json.
#
# Usage: scripts/assemble-plugin-windows.sh <arch> <version> <out-dir>
#   <arch>     Windows architecture: amd64 or arm64
#   <version>  release version without the leading "v" (e.g. 0.30.0)
#   <out-dir>  destination directory (recreated)
#
# The release workflow zips the result as ghost-plugin-windows-<arch>.zip.

set -euo pipefail

ARCH="${1:?usage: assemble-plugin-windows.sh <arch> <version> <out-dir>}"
VERSION="${2:?usage: assemble-plugin-windows.sh <arch> <version> <out-dir>}"
OUT="${3:?usage: assemble-plugin-windows.sh <arch> <version> <out-dir>}"

case "$ARCH" in
  amd64|arm64) ;;
  *)
    echo "error: unsupported Windows architecture '$ARCH' (expected amd64 or arm64)" >&2
    exit 1
    ;;
esac

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

rm -rf "$OUT"
mkdir -p "$OUT"
cp -R "$ROOT/plugin-windows/." "$OUT/"

# Stamp the release version (so `/plugin update` sees each release as new) and
# the arch-qualified plugin name (so the archive matches its marketplace
# entry). The sed fallback anchors on top-level indentation: author.name is
# nested deeper and must not match.
MANIFEST="$OUT/.claude-plugin/plugin.json"
if command -v jq >/dev/null 2>&1; then
  jq --arg v "$VERSION" --arg n "ghost-windows-$ARCH" \
    '.version = $v | .name = $n' "$MANIFEST" >"$MANIFEST.tmp"
  mv "$MANIFEST.tmp" "$MANIFEST"
else
  sed -i.bak -E \
    -e 's/^  "version": "[^"]*"/  "version": "'"$VERSION"'"/' \
    -e 's/^  "name": "[^"]*"/  "name": "ghost-windows-'"$ARCH"'"/' \
    "$MANIFEST"
  rm -f "$MANIFEST.bak"
fi

cd "$ROOT"
echo "building windows-$ARCH"
mkdir -p "$OUT/bin"
CGO_ENABLED=0 GOOS=windows GOARCH="$ARCH" \
  go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$OUT/bin/ghost.exe" ./cmd/ghost

# The stamped manifest must still be valid JSON.
python3 -c "import json,sys; json.load(open('$MANIFEST'))" \
  || { echo "error: assembled plugin.json is not valid JSON" >&2; exit 1; }

echo "assembled ghost windows-$ARCH plugin $VERSION at $OUT"
