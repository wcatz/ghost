#!/usr/bin/env bash
# Usage: make-unit-config.sh <unit-run-id> <ghost-wrapped-path> <ghost-bin-path>
# Writes mcp.json + settings.json for an isolated `claude -p` eval session
# under /tmp/ghost-eval/<unit-run-id>/claude-config/, and pre-creates the
# ghost-wrapped data/config dirs for that same unit-run-id.
set -euo pipefail

UNIT_RUN_ID="${1:?usage: make-unit-config.sh <unit-run-id> <ghost-wrapped-path> <ghost-bin-path>}"
GHOST_WRAPPED="$(realpath "${2:?missing ghost-wrapped path}")"
GHOST_BIN="$(realpath "${3:?missing ghost binary path}")"

if ! [[ "${UNIT_RUN_ID}" =~ ^[A-Za-z0-9_-]+$ ]]; then
  echo "error: unit-run-id must match ^[A-Za-z0-9_-]+\$, got '${UNIT_RUN_ID}'" >&2
  exit 1
fi

# Defense-in-depth JSON string escaping for the interpolated values below —
# UNIT_RUN_ID is charset-validated above, but GHOST_WRAPPED/GHOST_BIN are
# realpath'd filesystem paths that could still contain a quote or backslash.
json_escape() {
  local s="$1"
  s="${s//\\/\\\\}"
  s="${s//\"/\\\"}"
  printf '%s' "$s"
}
GHOST_WRAPPED_JSON="$(json_escape "${GHOST_WRAPPED}")"
GHOST_BIN_JSON="$(json_escape "${GHOST_BIN}")"
UNIT_RUN_ID_JSON="$(json_escape "${UNIT_RUN_ID}")"

UNIT_ROOT="/tmp/ghost-eval/${UNIT_RUN_ID}"
CONFIG_DIR="${UNIT_ROOT}/claude-config"
mkdir -p "${CONFIG_DIR}" "${UNIT_ROOT}/data" "${UNIT_ROOT}/config"

cat > "${CONFIG_DIR}/mcp.json" <<EOF
{
  "mcpServers": {
    "ghost": {
      "command": "${GHOST_WRAPPED_JSON}",
      "args": ["${UNIT_RUN_ID_JSON}", "${GHOST_BIN_JSON}", "mcp"]
    }
  }
}
EOF

cat > "${CONFIG_DIR}/settings.json" <<EOF
{
  "hooks": {
    "SessionStart": [
      {
        "matcher": "",
        "hooks": [
          {
            "type": "command",
            "command": "\"${GHOST_WRAPPED_JSON}\" \"${UNIT_RUN_ID_JSON}\" \"${GHOST_BIN_JSON}\" hook session-start"
          }
        ]
      }
    ]
  }
}
EOF

echo "unit config ready: ${CONFIG_DIR}" >&2
