#!/usr/bin/env bash
#
# assemble-plugin.sh assembles the Claude Code plugin tree from the repo's
# static plugin/ files plus freshly cross-compiled ghost binaries, and stamps
# the release version into the plugin manifest. Called by the release workflow;
# runnable by hand for local validation.
#
# The plugin is one artifact covering all six goreleaser platforms: the user
# picks their platform once (user_config.platform) and Claude Code substitutes
# it into the MCP and hook commands. See
# docs/superpowers/specs/2026-08-20-ghost-claude-plugin-design.md.
#
# Usage: scripts/assemble-plugin.sh <version> <out-dir>
#   <version>  release version without the leading "v" (e.g. 0.30.0)
#   <out-dir>  destination directory (recreated)
#
# PLATFORMS may be overridden for a fast local check, e.g.
#   PLATFORMS="linux-amd64" scripts/assemble-plugin.sh 0.0.0 /tmp/ghost-plugin

set -euo pipefail

VERSION="${1:?usage: assemble-plugin.sh <version> <out-dir>}"
OUT="${2:?usage: assemble-plugin.sh <version> <out-dir>}"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

read -r -a PLATFORMS <<<"${PLATFORMS:-darwin-arm64 darwin-amd64 linux-arm64 linux-amd64 windows-arm64 windows-amd64}"

rm -rf "$OUT"
mkdir -p "$OUT"
cp -R "$ROOT/plugin/." "$OUT/"

# Stamp the release version so `/plugin update` sees each release as new.
MANIFEST="$OUT/.claude-plugin/plugin.json"
if command -v jq >/dev/null 2>&1; then
  jq --arg v "$VERSION" '.version = $v' "$MANIFEST" >"$MANIFEST.tmp"
  mv "$MANIFEST.tmp" "$MANIFEST"
else
  sed -i.bak -E 's/"version": "[^"]*"/"version": "'"$VERSION"'"/' "$MANIFEST"
  rm -f "$MANIFEST.bak"
fi

cd "$ROOT"
for p in "${PLATFORMS[@]}"; do
  os="${p%%-*}"
  arch="${p##*-}"
  ext=""
  [ "$os" = "windows" ] && ext=".exe"
  bin="$OUT/bin/$p/ghost$ext"
  mkdir -p "$(dirname "$bin")"
  echo "building $p"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$bin" ./cmd/ghost
done

# The stamped manifest must still be valid JSON.
python3 -c "import json,sys; json.load(open('$MANIFEST'))" \
  || { echo "error: assembled plugin.json is not valid JSON" >&2; exit 1; }

echo "assembled ghost plugin $VERSION at $OUT"
