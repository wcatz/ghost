#!/usr/bin/env bash
# Usage: claude-eval-session.sh <unit-run-id> <prompt>
# Launches one isolated `claude -p` session against the scratch ghost
# instance for <unit-run-id> (config must already exist — run
# make-unit-config.sh first). --setting-sources excludes "user" so the
# real, unwrapped SessionStart hook in ~/.claude/settings.json never loads;
# --settings supplies the scratch-wrapped replacement instead.
set -euo pipefail

UNIT_RUN_ID="${1:?usage: claude-eval-session.sh <unit-run-id> <prompt>}"
PROMPT="${2:?missing prompt}"

CONFIG_DIR="/tmp/ghost-eval/${UNIT_RUN_ID}/claude-config"

claude -p \
  --mcp-config "${CONFIG_DIR}/mcp.json" \
  --strict-mcp-config \
  --settings "${CONFIG_DIR}/settings.json" \
  --setting-sources project,local \
  --permission-mode bypassPermissions \
  "${PROMPT}"
