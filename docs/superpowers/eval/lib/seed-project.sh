#!/usr/bin/env bash
# Usage: seed-project.sh <real-db-path> <project-identifier> <scratch-db-path>
# Copies one project's `projects` row and its `memories` rows from the real
# DB into a scratch DB that ghost has already initialized (so the schema,
# including FTS5 triggers, already exists there).
# The project identifier may be either the project's `name` (what the ghost
# CLI takes, e.g. `ghost reflect ghost`) or its raw `id`. Real projects
# registered by absolute path have a hashed id that differs from their name
# (id=6bdc098af7f5, name=ghost), so the identifier is resolved to the real id
# first and everything is filtered by THAT id.
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

# The identifier is interpolated once, inside the resolving subquery; every
# other statement filters on the resolved id, never on the raw argument.
sqlite3 "${SCRATCH_DB}" <<SQL
ATTACH DATABASE 'file:${REAL_DB_URI}?mode=ro' AS real_db;

CREATE TEMP TABLE seed_target AS
SELECT id FROM real_db.projects
WHERE name = '${PROJECT_ID}' OR id = '${PROJECT_ID}'
ORDER BY (name = '${PROJECT_ID}') DESC
LIMIT 1;

INSERT INTO projects (id, path, name, created_at, updated_at)
SELECT id, path, name, created_at, updated_at
FROM real_db.projects
WHERE id IN (SELECT id FROM seed_target)
ON CONFLICT(id) DO NOTHING;

INSERT INTO memories (id, project_id, category, content, importance,
                       access_count, last_accessed, source, tags, pinned,
                       created_at, updated_at, resolved_at)
SELECT id, project_id, category, content, importance,
       access_count, last_accessed, source, tags, pinned,
       created_at, updated_at, resolved_at
FROM real_db.memories
WHERE project_id IN (SELECT id FROM seed_target)
  AND resolved_at IS NULL;

DROP TABLE seed_target;
DETACH DATABASE real_db;
SQL

# An empty copy is the failure this script used to hide: the seeded DB looked
# fine and every downstream `ghost <cmd> <project>` reported "project not
# found" instead. Fail loudly here instead.
SEEDED_PROJECTS="$(sqlite3 "${SCRATCH_DB}" "SELECT count(*) FROM projects WHERE name = '${PROJECT_ID}' OR id = '${PROJECT_ID}';")"
if [ "${SEEDED_PROJECTS}" -eq 0 ]; then
  echo "error: no project matching '${PROJECT_ID}' (by name or id) exists in ${REAL_DB} — nothing seeded" >&2
  exit 1
fi

SEEDED_MEMORIES="$(sqlite3 "${SCRATCH_DB}" "SELECT count(*) FROM memories WHERE project_id IN (SELECT id FROM projects WHERE name = '${PROJECT_ID}' OR id = '${PROJECT_ID}');")"
if [ "${SEEDED_MEMORIES}" -eq 0 ]; then
  echo "error: project '${PROJECT_ID}' seeded 0 memories from ${REAL_DB} — refusing to run an eval against an empty corpus" >&2
  exit 1
fi

echo "seeded ${SCRATCH_DB} with ${PROJECT_ID} (${SEEDED_MEMORIES} memories) from ${REAL_DB}" >&2
