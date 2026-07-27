#!/usr/bin/env bash
# Usage: seed-project.sh <real-db-path> <project-id> <scratch-db-path>
# Copies one project's `projects` row and its `memories` rows from the real
# DB into a scratch DB that ghost has already initialized (so the schema,
# including FTS5 triggers, already exists there).
set -euo pipefail

REAL_DB="${1:?usage: seed-project.sh <real-db-path> <project-id> <scratch-db-path>}"
PROJECT_ID="${2:?missing project-id}"
SCRATCH_DB="${3:?missing scratch-db-path}"

sqlite3 "${SCRATCH_DB}" <<SQL
ATTACH DATABASE '${REAL_DB}' AS real_db;

INSERT INTO projects (id, path, name, created_at, updated_at)
SELECT id, path, name, created_at, updated_at
FROM real_db.projects
WHERE id = '${PROJECT_ID}'
ON CONFLICT(id) DO NOTHING;

INSERT INTO memories (id, project_id, category, content, importance,
                       access_count, last_accessed, source, tags, pinned,
                       created_at, updated_at, resolved_at)
SELECT id, project_id, category, content, importance,
       access_count, last_accessed, source, tags, pinned,
       created_at, updated_at, resolved_at
FROM real_db.memories
WHERE project_id = '${PROJECT_ID}'
  AND resolved_at IS NULL;

DETACH DATABASE real_db;
SQL

echo "seeded ${SCRATCH_DB} with ${PROJECT_ID} from ${REAL_DB}" >&2
