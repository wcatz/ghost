#!/usr/bin/env bash
# Usage: make-unit-config.sh <unit-run-id> <ghost-wrapped-path> <ghost-bin-path>
# Writes mcp.json + settings.json for an isolated `claude -p` eval session
# under /tmp/ghost-eval/<unit-run-id>/claude-config/, and pre-creates the
# ghost-wrapped data/config dirs for that same unit-run-id.
set -euo pipefail

UNIT_RUN_ID="${1:?usage: make-unit-config.sh <unit-run-id> <ghost-wrapped-path> <ghost-bin-path>}"
GHOST_WRAPPED="$(realpath "${2:?missing ghost-wrapped path}")"
GHOST_BIN="$(realpath "${3:?missing ghost binary path}")"

UNIT_ROOT="/tmp/ghost-eval/${UNIT_RUN_ID}"
CONFIG_DIR="${UNIT_ROOT}/claude-config"
mkdir -p "${CONFIG_DIR}" "${UNIT_ROOT}/data" "${UNIT_ROOT}/config"

cat > "${CONFIG_DIR}/mcp.json" <<EOF
{
  "mcpServers": {
    "ghost": {
      "command": "${GHOST_WRAPPED}",
      "args": ["${UNIT_RUN_ID}", "${GHOST_BIN}", "mcp"]
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
            "command": "\"${GHOST_WRAPPED}\" \"${UNIT_RUN_ID}\" \"${GHOST_BIN}\" hook session-start"
          }
        ]
      }
    ]
  }
}
EOF

echo "unit config ready: ${CONFIG_DIR}" >&2
