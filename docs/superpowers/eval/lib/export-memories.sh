#!/usr/bin/env bash
# Usage: export-memories.sh <real-db-path> <project-id> <output-jsonl-path>
# Read-only dump of a project's current memories, for diffing against
# what a live replay agent chooses to save.
set -euo pipefail

REAL_DB="${1:?usage: export-memories.sh <real-db-path> <project-id> <output-jsonl-path>}"
PROJECT_ID="${2:?missing project-id}"
OUT_PATH="${3:?missing output path}"

if ! [[ "${PROJECT_ID}" =~ ^[A-Za-z0-9_-]+$ ]]; then
  echo "error: project-id must match ^[A-Za-z0-9_-]+\$, got '${PROJECT_ID}'" >&2
  exit 1
fi

mkdir -p "$(dirname "${OUT_PATH}")"

# The `>` redirection below truncates OUT_PATH before sqlite3 even runs, so if
# OUT_PATH resolved to REAL_DB this would destroy the real database first —
# check via canonical paths (realpath -m tolerates OUT_PATH not existing yet).
REAL_DB_ABS="$(realpath "${REAL_DB}")"
OUT_PATH_ABS="$(realpath -m "${OUT_PATH}")"
if [ "${OUT_PATH_ABS}" = "${REAL_DB_ABS}" ]; then
  echo "error: output path resolves to the real DB path (${REAL_DB_ABS}) — refusing to overwrite it" >&2
  exit 1
fi

sqlite3 -readonly "${REAL_DB}" <<SQL > "${OUT_PATH}"
.mode list
select json_object('category', category, 'content', content, 'importance', importance, 'source', source, 'tags', tags)
from memories
where project_id = '${PROJECT_ID}'
  and resolved_at is null
order by created_at;
SQL

echo "exported $(wc -l < "${OUT_PATH}") memories for ${PROJECT_ID} -> ${OUT_PATH}" >&2
