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

# Resolve PROJECT_ID to a single real project id before touching the scratch
# DB at all. An exact id match is authoritative (id is the primary key, so it
# can never be ambiguous); only fall back to a name match when no id matches,
# and refuse to guess if that name is itself ambiguous (defense in depth —
# name has no uniqueness constraint in the schema).
ID_MATCH_COUNT="$(sqlite3 "${REAL_DB}" "SELECT count(*) FROM projects WHERE id = '${PROJECT_ID}';")"
if [ "${ID_MATCH_COUNT}" -eq 1 ]; then
  RESOLVED_ID="${PROJECT_ID}"
else
  NAME_MATCH_COUNT="$(sqlite3 "${REAL_DB}" "SELECT count(*) FROM projects WHERE name = '${PROJECT_ID}';")"
  if [ "${NAME_MATCH_COUNT}" -eq 0 ]; then
    echo "error: no project matching '${PROJECT_ID}' (by name or id) exists in ${REAL_DB} — nothing seeded" >&2
    exit 1
  elif [ "${NAME_MATCH_COUNT}" -gt 1 ]; then
    echo "error: project name '${PROJECT_ID}' matches ${NAME_MATCH_COUNT} projects in ${REAL_DB} — ambiguous, refusing to guess" >&2
    exit 1
  fi
  RESOLVED_ID="$(sqlite3 "${REAL_DB}" "SELECT id FROM projects WHERE name = '${PROJECT_ID}';")"
fi

if ! [[ "${RESOLVED_ID}" =~ ^[A-Za-z0-9_-]+$ ]]; then
  echo "error: resolved project id must match ^[A-Za-z0-9_-]+\$, got '${RESOLVED_ID}'" >&2
  exit 1
fi

# Every statement below filters on RESOLVED_ID, which is now a single
# unambiguous real id — no OR name-match, no ORDER BY tie-break needed.
sqlite3 "${SCRATCH_DB}" <<SQL
ATTACH DATABASE 'file:${REAL_DB_URI}?mode=ro' AS real_db;

INSERT INTO projects (id, path, name, created_at, updated_at)
SELECT id, path, name, created_at, updated_at
FROM real_db.projects
WHERE id = '${RESOLVED_ID}'
ON CONFLICT(id) DO NOTHING;

INSERT INTO memories (id, project_id, category, content, importance,
                       access_count, last_accessed, source, tags, pinned,
                       created_at, updated_at, resolved_at)
SELECT id, project_id, category, content, importance,
       access_count, last_accessed, source, tags, pinned,
       created_at, updated_at, resolved_at
FROM real_db.memories
WHERE project_id = '${RESOLVED_ID}'
  AND resolved_at IS NULL;

DETACH DATABASE real_db;
SQL

# An empty copy is the failure this script used to hide: the seeded DB looked
# fine and every downstream `ghost <cmd> <project>` reported "project not
# found" instead. Fail loudly here instead. Both checks target RESOLVED_ID
# directly (not the raw argument), and the memory count matches the
# resolved_at IS NULL filter used above so a fully-resolved corpus can't pass.
SEEDED_PROJECTS="$(sqlite3 "${SCRATCH_DB}" "SELECT count(*) FROM projects WHERE id = '${RESOLVED_ID}';")"
if [ "${SEEDED_PROJECTS}" -eq 0 ]; then
  echo "error: project id '${RESOLVED_ID}' was not copied into ${SCRATCH_DB} — nothing seeded" >&2
  exit 1
fi

SEEDED_MEMORIES="$(sqlite3 "${SCRATCH_DB}" "SELECT count(*) FROM memories WHERE project_id = '${RESOLVED_ID}' AND resolved_at IS NULL;")"
if [ "${SEEDED_MEMORIES}" -eq 0 ]; then
  echo "error: project '${PROJECT_ID}' seeded 0 unresolved memories from ${REAL_DB} — refusing to run an eval against an empty corpus" >&2
  exit 1
fi

echo "seeded ${SCRATCH_DB} with ${PROJECT_ID} (resolved id ${RESOLVED_ID}, ${SEEDED_MEMORIES} memories) from ${REAL_DB}" >&2
