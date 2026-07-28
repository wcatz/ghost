#!/usr/bin/env bash
# Usage: seed-project.sh <real-db-path> <project-id> <scratch-db-path>
# Copies one project's `projects` row and its `memories` rows from the real
# DB into a scratch DB that ghost has already initialized (so the schema,
# including FTS5 triggers, already exists there).
# NOTE: memory_embeddings are NOT copied — seeded rows have no vector until
# re-embedded, so hybrid search degrades to FTS5-only for this corpus.
set -euo pipefail

REAL_DB="${1:?usage: seed-project.sh <real-db-path> <project-id> <scratch-db-path>}"
PROJECT_ID="${2:?missing project-id}"
SCRATCH_DB="${3:?missing scratch-db-path}"

if ! [[ "${PROJECT_ID}" =~ ^[A-Za-z0-9_-]+$ ]]; then
  echo "error: project-id must match ^[A-Za-z0-9_-]+\$, got '${PROJECT_ID}'" >&2
  exit 1
fi

# SCRATCH_DB is opened writable below and has REAL_DB rows INSERTed into it —
# if it resolved to the same file as REAL_DB (e.g. a bad scratch-root
# override), this would corrupt the real database.
REAL_DB_ABS="$(realpath "${REAL_DB}")"
SCRATCH_DB_ABS="$(realpath -m "${SCRATCH_DB}")"
if [ "${SCRATCH_DB_ABS}" = "${REAL_DB_ABS}" ]; then
  echo "error: scratch DB path resolves to the real DB path (${REAL_DB_ABS}) — refusing to write to it" >&2
  exit 1
fi

# REAL_DB_ABS is a filesystem path used inside a sqlite URI; percent-encode
# chars that are structurally significant in a URI (% ? # ') so a path
# containing one of them can't break out of the query-string/fragment or the
# single-quoted SQL literal it's embedded in.
REAL_DB_URI="$(printf '%s' "${REAL_DB_ABS}" | sed -e "s/%/%25/g" -e "s/?/%3F/g" -e "s/#/%23/g" -e "s/'/%27/g")"

sqlite3 "${SCRATCH_DB}" <<SQL
ATTACH DATABASE 'file:${REAL_DB_URI}?mode=ro' AS real_db;

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
