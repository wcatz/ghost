#!/usr/bin/env bash
#
# assemble-plugin.sh assembles the Claude Code plugin tree from the repo's
# static plugin/ files plus freshly cross-compiled ghost binaries, and stamps
# the release version into the plugin manifest. Called by the release workflow;
# runnable by hand for local validation.
#
# The plugin is one artifact covering the goreleaser darwin/linux platforms: a
# POSIX launcher (bin/ghost-launcher) resolves the host platform from uname at
# runtime and execs the matching bundled binary, so no userConfig value is
# required. Native Windows cannot exec the POSIX launcher, so it ships as two
# separate per-architecture archives assembled by the sibling
# scripts/assemble-plugin-windows.sh (see that script's header for why).
# See docs/superpowers/specs/2026-08-20-ghost-claude-plugin-design.md.
#
# Usage: scripts/assemble-plugin.sh <version> <out-dir>
#   <version>  release version without the leading "v" (e.g. 0.30.0)
#   <out-dir>  destination directory (recreated)
#
# PLATFORMS may be overridden for a fast local check, e.g.
#   PLATFORMS="linux-amd64" scripts/assemble-plugin.sh 0.0.0 /tmp/ghost-plugin
#
# The checked-in plugin manifests carry version 0.0.0 so a source-checkout
# install (`claude --plugin-dir plugin`) is honest about having no release
# version; this script stamps the real release version into the assembled
# copy (assemble-plugin-windows.sh stamps the version and the arch-qualified
# name into its manifest).

set -euo pipefail

VERSION="${1:?usage: assemble-plugin.sh <version> <out-dir>}"
OUT="${2:?usage: assemble-plugin.sh <version> <out-dir>}"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

read -r -a PLATFORMS <<<"${PLATFORMS:-darwin-arm64 darwin-amd64 linux-arm64 linux-amd64}"

rm -rf "$OUT"
mkdir -p "$OUT"
cp -R "$ROOT/plugin/." "$OUT/"

# The MCP server and hooks exec the launcher directly, so it must carry the
# exec bit through the archive. A plain copy preserves it, but restore it
# explicitly so a checkout or staging step that drops modes cannot ship a
# non-executable entry point.
LAUNCHER="$OUT/bin/ghost-launcher"
chmod +x "$LAUNCHER"
[ -x "$LAUNCHER" ] || { echo "error: assembled launcher is not executable: $LAUNCHER" >&2; exit 1; }

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

# The stamped manifest must still be valid JSON. Resolve python3 or python:
# Windows runners (Git Bash) expose only `python`.
PY="$(command -v python3 || command -v python || true)"
[ -n "$PY" ] || { echo "error: python3 or python is required to validate the manifest" >&2; exit 1; }
"$PY" -c "import json,sys; json.load(open('$MANIFEST'))" \
  || { echo "error: assembled plugin.json is not valid JSON" >&2; exit 1; }

echo "assembled ghost plugin $VERSION at $OUT"
