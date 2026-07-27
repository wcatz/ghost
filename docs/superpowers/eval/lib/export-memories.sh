#!/usr/bin/env bash
# Usage: export-memories.sh <real-db-path> <project-id> <output-jsonl-path>
# Read-only dump of a project's current memories, for diffing against
# what a live replay agent chooses to save.
set -euo pipefail

REAL_DB="${1:?usage: export-memories.sh <real-db-path> <project-id> <output-jsonl-path>}"
PROJECT_ID="${2:?missing project-id}"
OUT_PATH="${3:?missing output path}"

sqlite3 -readonly "${REAL_DB}" <<SQL > "${OUT_PATH}"
.mode json
select category, content, importance, source, tags
from memories
where project_id = '${PROJECT_ID}'
  and resolved_at is null
order by created_at;
SQL

echo "exported $(wc -l < "${OUT_PATH}") memories for ${PROJECT_ID} -> ${OUT_PATH}" >&2
