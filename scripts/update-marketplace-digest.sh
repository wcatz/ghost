#!/usr/bin/env bash
#
# update-marketplace-digest.sh pins every marketplace entry to the exact
# bytes this release published: a version-pinned
# releases/download/v<X>/<archive> URL plus the archive's sha256, so the
# catalog always references one immutable asset set. Called by the release
# workflow after the three plugin archives are attached and before the draft
# is published; runnable by hand for local validation.
# See docs/superpowers/specs/2026-08-20-ghost-claude-plugin-design.md.
#
# Why version-pinned + digest: releases/latest/download retargets on every
# release (404 window while assets attach, undetectable --clobber swaps
# after), while a pinned URL names one asset set forever and the digest makes
# a swapped asset fail at install time instead of landing silently.
#
# jq is hard-required here (no python fallback like the assemble scripts):
# this runs only in release CI and local validation, both of which have jq.
#
# Usage: scripts/update-marketplace-digest.sh <version> <zip-dir>
#   <version>  release version without the leading "v" (e.g. 0.30.0)
#   <zip-dir>  directory holding the three release archives (e.g. dist):
#              ghost-plugin.zip, ghost-plugin-windows-amd64.zip,
#              ghost-plugin-windows-arm64.zip
#
# MARKETPLACE_FILE overrides the catalog path (default:
# <repo-root>/.claude-plugin/marketplace.json) so local proofs can rewrite a
# disposable copy; the release workflow never sets it, so CI always pins the
# repo-root file.

set -euo pipefail

VERSION="${1:?usage: update-marketplace-digest.sh <version> <zip-dir>}"
ZIPDIR="${2:?usage: update-marketplace-digest.sh <version> <zip-dir>}"

if ! [[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "error: version must be bare semver without the leading 'v' (got '$VERSION')" >&2
  exit 1
fi

command -v jq >/dev/null 2>&1 || { echo "error: jq is required" >&2; exit 1; }

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FILE="${MARKETPLACE_FILE:-$ROOT/.claude-plugin/marketplace.json}"
[ -f "$FILE" ] || { echo "error: marketplace file not found: $FILE" >&2; exit 1; }
[ -d "$ZIPDIR" ] || { echo "error: zip directory not found: $ZIPDIR" >&2; exit 1; }

# Repository the version-pinned URLs point at: release CI exports
# GITHUB_REPOSITORY (so the catalog and the workflow's verify step always
# use the same slug); the literal fallback is for local validation, which
# runs without Actions env.
REPO_URL="https://github.com/${GITHUB_REPOSITORY:-wcatz/ghost}"

# Marketplace entry -> archive name. One mapping, one place: entry `ghost`
# ships the POSIX archive; `ghost-windows-<arch>` ships that arch's archive
# (the release workflow zips them under exactly these names).
zip_for_entry() {
  case "$1" in
    ghost) echo "ghost-plugin.zip" ;;
    ghost-windows-amd64|ghost-windows-arm64)
      echo "ghost-plugin-windows-${1#ghost-windows-}.zip"
      ;;
    *) return 1 ;;
  esac
}

# Resolve the checksum tool the way the assemble scripts resolve python:
# sha256sum on Linux/Git Bash, shasum on macOS.
if command -v sha256sum >/dev/null 2>&1; then
  SHA256_CMD=(sha256sum)
elif command -v shasum >/dev/null 2>&1; then
  SHA256_CMD=(shasum -a 256)
else
  echo "error: sha256sum or shasum is required" >&2
  exit 1
fi

jq -e '.plugins | type == "array"' "$FILE" >/dev/null 2>&1 \
  || { echo "error: $FILE is not valid marketplace JSON (no plugins array)" >&2; exit 1; }

mapfile -t ENTRIES < <(jq -r '.plugins[].name' "$FILE")
if [ "${#ENTRIES[@]}" -ne 3 ]; then
  echo "error: expected exactly 3 marketplace entries, found ${#ENTRIES[@]}: ${ENTRIES[*]:-none}" >&2
  exit 1
fi

# Digest every archive and accumulate {entry: {zip, sha256}} for the rewrite.
PINS='{}'
for entry in "${ENTRIES[@]}"; do
  zip="$(zip_for_entry "$entry")" || {
    echo "error: unknown marketplace entry '$entry' (expected ghost, ghost-windows-amd64, or ghost-windows-arm64)" >&2
    exit 1
  }
  path="$ZIPDIR/$zip"
  [ -f "$path" ] || { echo "error: missing archive for entry '$entry': $path" >&2; exit 1; }
  sha="$("${SHA256_CMD[@]}" "$path" | awk '{print $1}')"
  PINS="$(jq -c --arg e "$entry" --arg z "$zip" --arg s "$sha" \
    '. + {($e): {zip: $z, sha256: $s}}' <<<"$PINS")"
  echo "pinned $entry -> $zip sha256=$sha"
done

# Rewrite the catalog: version-pinned URL + sha256 per entry. jq's default
# 2-space indent and key order preservation keep the diff to exactly the
# url/sha256 lines.
TMP="$FILE.tmp.$$"
trap 'rm -f "$TMP"' EXIT
jq --arg v "$VERSION" --arg base "$REPO_URL" --argjson pins "$PINS" '
  .plugins |= map(
    .source.url = ($base + "/releases/download/v" + $v + "/" + $pins[.name].zip)
    | .source.sha256 = $pins[.name].sha256
  )' "$FILE" >"$TMP"

# Assertions before writing — fail loudly rather than commit a bad catalog.
jq -e '.plugins | length == 3' "$TMP" >/dev/null \
  || { echo "error: rewritten catalog does not have exactly 3 entries" >&2; exit 1; }
jq -e --arg needle "releases/download/v${VERSION}/" \
  '[.plugins[].source.url | contains($needle)] | all' "$TMP" >/dev/null \
  || { echo "error: not every entry url is pinned to v${VERSION}" >&2; exit 1; }
jq -e '[.plugins[].source.sha256 | test("^[0-9a-f]{64}$")] | all' "$TMP" >/dev/null \
  || { echo "error: not every entry sha256 is 64 lowercase hex chars" >&2; exit 1; }
jq -e . "$TMP" >/dev/null \
  || { echo "error: rewritten marketplace file is not valid JSON" >&2; exit 1; }

if cmp -s "$TMP" "$FILE"; then
  echo "note: $FILE already pinned for v${VERSION} (byte-identical; rewriting anyway, idempotent)"
else
  echo "pinning $FILE to v${VERSION} (url + sha256 for ${#ENTRIES[@]} entries)"
fi
mv "$TMP" "$FILE"
trap - EXIT
echo "updated $FILE"
