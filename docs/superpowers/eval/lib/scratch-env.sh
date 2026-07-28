#!/usr/bin/env bash
# Usage: source scratch-env.sh <run-id>
# Creates /tmp/ghost-eval/<run-id>/{data,config} and exports the XDG vars
# so any `ghost` invocation in this shell reads/writes the scratch dir.
set -euo pipefail

RUN_ID="${1:?usage: source scratch-env.sh <run-id>}"
GHOST_EVAL_ROOT="/tmp/ghost-eval/${RUN_ID}"

mkdir -p "${GHOST_EVAL_ROOT}/data" "${GHOST_EVAL_ROOT}/config"

export GHOST_EVAL_ROOT
export XDG_DATA_HOME="${GHOST_EVAL_ROOT}/data"
export XDG_CONFIG_HOME="${GHOST_EVAL_ROOT}/config"
# ANTHROPIC_API_KEY is intentionally left as-is (inherited from the parent
# shell) — ghost reads it directly from the environment, so no copy is needed.

echo "scratch env ready: ${GHOST_EVAL_ROOT}" >&2
