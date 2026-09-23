#!/usr/bin/env bash
# ARCHIVED: this helper belongs to the pre-CLI-migration real-world eval.
# Use eval/cycle for the current graded evaluation.
# Usage: claude-eval-session.sh <unit-run-id> <prompt> <project-basename>
# Launches one isolated `claude -p` session against the scratch ghost
# instance for <unit-run-id> (config must already exist — run
# make-unit-config.sh first). --setting-sources excludes "user" so the
# real, unwrapped SessionStart hook in ~/.claude/settings.json never loads;
# --settings supplies the scratch-wrapped replacement instead.
#
# <project-basename> must equal the target ghost project_id: Ghost's
# SessionStart hook (internal/mcpinit/hook.go lookupProject) matches a
# project by cwd path or by `name = basename(cwd)`, and ghost_memory_save
# with only a project_id sets that project's stored name/path to the id
# itself. So the subprocess is launched with cwd set to a scratch directory
# whose basename is <project-basename>, making the hook's basename match
# fire correctly instead of matching this repo's own cwd. This also keeps
# the subprocess's session transcript out of the real
# ~/.claude/projects/-home-wayne-git-ghost/ directory.
#
# ANTHROPIC_API_KEY is deliberately unset before invoking `claude -p` below.
# If present, it overrides Claude Code's subscription/OAuth login and bills
# this actor session as pay-per-token API usage instead of the subscription.
# Ghost's own reflect/resolve/supersede calls now use the configured CLI
# harness as well; the unset keeps this archived actor session from choosing
# a different billing path.
set -euo pipefail

UNIT_RUN_ID="${1:?usage: claude-eval-session.sh <unit-run-id> <prompt> <project-basename>}"
PROMPT="${2:?missing prompt}"
PROJECT_BASENAME="${3:?missing project-basename}"

CONFIG_DIR="/tmp/ghost-eval/${UNIT_RUN_ID}/claude-config"
SESSION_CWD="/tmp/ghost-eval/${UNIT_RUN_ID}/cwd/${PROJECT_BASENAME}"
mkdir -p "${SESSION_CWD}"

cd "${SESSION_CWD}"

env -u ANTHROPIC_API_KEY claude -p \
  --mcp-config "${CONFIG_DIR}/mcp.json" \
  --strict-mcp-config \
  --settings "${CONFIG_DIR}/settings.json" \
  --setting-sources project,local \
  --permission-mode bypassPermissions \
  "${PROMPT}"
