package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// validCategories is the single Go-side source of truth for the memories
// category CHECK constraint in initSQL (and migrate.go's mirrored CHECK).
// Writers that accept caller-supplied categories (reflection parse, MCP save)
// validate against this set so an invalid value is normalized or rejected
// before it can fail the whole INSERT/transaction on the SQL CHECK.
var validCategories = map[string]bool{
	"architecture": true,
	"decision":     true,
	"pattern":      true,
	"convention":   true,
	"gotcha":       true,
	"dependency":   true,
	"preference":   true,
	"fact":         true,
}

// IsValidCategory reports whether cat is one of the eight canonical memory
// categories enforced by the schema CHECK.
func IsValidCategory(cat string) bool {
	return validCategories[cat]
}

// initSQL is the schema for the ghost database — the single source of truth.
// Kept as a Go constant rather than go:embed because embed paths cannot use "..".
// Note: CREATE TABLE IF NOT EXISTS never migrates an existing database — a new
// column or CHECK value only reaches DBs created after the change. When you
// alter an existing table here, bump schemaVersion and append a migration
// step in migrate.go so existing databases receive it too.
const initSQL = `
CREATE TABLE IF NOT EXISTS projects (
    id          TEXT PRIMARY KEY,
    path        TEXT NOT NULL UNIQUE,
    name        TEXT NOT NULL,
    -- Normalized remote URL of the repository the project's path points at.
    -- Nullable: a project may have no repository, and a caller may have no
    -- path to inspect. This is what makes ~/src/ghost and ~/work/ghost one
    -- project rather than two — path identity cannot say they are the same.
    -- Only the remote is stored; provider/owner/name are derived from it, so
    -- they can never drift out of step when a remote is rewritten.
    repo_remote TEXT,
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS memories (
    id            TEXT PRIMARY KEY DEFAULT (hex(randomblob(16))),
    project_id    TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    category      TEXT NOT NULL DEFAULT 'fact'
                  CHECK (category IN (
                      'architecture', 'decision', 'pattern', 'convention',
                      'gotcha', 'dependency', 'preference', 'fact'
                  )),
    content       TEXT NOT NULL,
    importance    REAL NOT NULL DEFAULT 0.5,
    access_count  INTEGER NOT NULL DEFAULT 0,
    last_accessed TEXT,
    source        TEXT NOT NULL DEFAULT 'reflection'
                  CHECK (source IN ('reflection', 'chat', 'manual', 'tool', 'mcp', 'onboarding', 'decision_log', 'builtin')),
    tags          TEXT DEFAULT '[]',
    pinned        INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at    TEXT NOT NULL DEFAULT (datetime('now')),
    resolved_at   TEXT,
    resolve_kept_hash TEXT NOT NULL DEFAULT '',
    valid_from    TEXT,
    valid_until   TEXT,
    verified_at   TEXT,
    agent         TEXT,
    session_id    TEXT,
    source_ref    TEXT,
    confidence    REAL,
    -- Machine-readable scope: a JSON object such as
    -- {"environment":"production","component":"api"}, or NULL when the memory
    -- applies everywhere. NULL and absent are deliberately the same thing —
    -- "no scope stated" must not read as a scope of "", and a fabricated
    -- scope would claim where knowledge applies that nobody asserted.
    scope         TEXT,
    -- Retention tier (schema v19, issue #587): how long this memory is wanted
    -- and what may be done to it. 'project' is the default and is what every
    -- row that predates the column reads after migration, because that is the
    -- life the corpus has always had. 'session' is a fact true of one
    -- conversation; Ghost derives expires_at for it on save and 'ghost prune'
    -- is the only thing that removes one, never automatically. 'persistent' is
    -- a user-declared keep-forever, exempt from consolidation, supersede,
    -- resolve and pruning.
    --
    -- NOT NULL with a CHECK rather than a nullable column: a reader that has to
    -- carry a NULL case for "no tier was ever decided" is carrying a case no
    -- writer can produce, and the default already answers it.
    retention     TEXT NOT NULL DEFAULT 'project'
                  CHECK (retention IN ('session', 'project', 'persistent')),
    -- When this memory stops being wanted. DERIVED for a session row (now plus
    -- SessionTTL at the save) and stated by nobody else: a project row expires
    -- only when somebody resolves or deletes it, and a persistent row never
    -- does. NULL therefore means "no expiry is claimed", and a row with a NULL
    -- expires_at is never a prune candidate — the unprunable direction.
    --
    -- Stored in the same 'YYYY-MM-DD HH:MM:SS' form as every other timestamp
    -- column here, because the prune compares it as text and a value in some
    -- other format simply does not match -- which leaves the row in the store
    -- rather than taking it out.
    expires_at    TEXT
);

CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(
    content,
    content=memories,
    content_rowid=rowid,
    tokenize='porter unicode61'
);

CREATE TRIGGER IF NOT EXISTS memories_ai AFTER INSERT ON memories BEGIN
    INSERT INTO memories_fts(rowid, content) VALUES (new.rowid, new.content);
END;

CREATE TRIGGER IF NOT EXISTS memories_ad AFTER DELETE ON memories BEGIN
    INSERT INTO memories_fts(memories_fts, rowid, content) VALUES('delete', old.rowid, old.content);
END;

CREATE TRIGGER IF NOT EXISTS memories_au AFTER UPDATE ON memories WHEN old.content != new.content BEGIN
    INSERT INTO memories_fts(memories_fts, rowid, content) VALUES('delete', old.rowid, old.content);
    INSERT INTO memories_fts(rowid, content) VALUES (new.rowid, new.content);
END;

CREATE INDEX IF NOT EXISTS idx_memories_project_cat ON memories(project_id, category);
CREATE INDEX IF NOT EXISTS idx_memories_project_imp ON memories(project_id, importance DESC);
CREATE INDEX IF NOT EXISTS idx_memories_project_source ON memories(project_id, source);

CREATE TABLE IF NOT EXISTS ghost_state (
    project_id          TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    interaction_count   INTEGER NOT NULL DEFAULT 0,
    learned_context     TEXT DEFAULT '',
    last_reflection_at  TEXT,
    reflection_summary  TEXT DEFAULT '',
    reflect_input_sig   TEXT NOT NULL DEFAULT '',
    updated_at          TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS token_usage (
    id              TEXT PRIMARY KEY DEFAULT (hex(randomblob(16))),
    project_id      TEXT NOT NULL,
    model           TEXT NOT NULL,
    input_tokens    INTEGER NOT NULL DEFAULT 0,
    output_tokens   INTEGER NOT NULL DEFAULT 0,
    cache_creation  INTEGER NOT NULL DEFAULT 0,
    cache_read      INTEGER NOT NULL DEFAULT 0,
    cost_usd        REAL NOT NULL DEFAULT 0,
    created_at      TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_usage_project ON token_usage(project_id, created_at DESC);

CREATE TABLE IF NOT EXISTS audit_log (
    id          TEXT PRIMARY KEY DEFAULT (hex(randomblob(16))),
    timestamp   TEXT NOT NULL DEFAULT (datetime('now')),
    action      TEXT NOT NULL,
    project_id  TEXT,
    user        TEXT DEFAULT '',
    details     TEXT DEFAULT '{}',
    tokens      INTEGER DEFAULT 0,
    cost_usd    REAL DEFAULT 0,
    duration_ms INTEGER DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_audit_project ON audit_log(project_id, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_audit_action ON audit_log(action, timestamp DESC);

CREATE TABLE IF NOT EXISTS memory_embeddings (
    memory_id   TEXT PRIMARY KEY REFERENCES memories(id) ON DELETE CASCADE,
    embedding   BLOB NOT NULL,
    model       TEXT NOT NULL DEFAULT 'nomic-embed-text',
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS tasks (
    id            TEXT PRIMARY KEY DEFAULT (hex(randomblob(16))),
    project_id    TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    title         TEXT NOT NULL,
    description   TEXT DEFAULT '',
    status        TEXT NOT NULL DEFAULT 'pending'
                  CHECK (status IN ('pending', 'active', 'done', 'blocked')),
    priority      INTEGER NOT NULL DEFAULT 2
                  CHECK (priority BETWEEN 0 AND 4),
    blocked_by    TEXT REFERENCES tasks(id) ON DELETE SET NULL,
    branch        TEXT DEFAULT '',
    pr_number     INTEGER,
    notes         TEXT DEFAULT '',
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at    TEXT NOT NULL DEFAULT (datetime('now')),
    completed_at  TEXT
);
CREATE INDEX IF NOT EXISTS idx_tasks_project_status ON tasks(project_id, status);

CREATE TABLE IF NOT EXISTS decisions (
    id                TEXT PRIMARY KEY DEFAULT (hex(randomblob(16))),
    project_id        TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    title             TEXT NOT NULL,
    decision          TEXT NOT NULL,
    alternatives      TEXT DEFAULT '[]',
    rationale         TEXT NOT NULL,
    status            TEXT NOT NULL DEFAULT 'active'
                      CHECK (status IN ('active', 'superseded', 'revisit')),
    superseded_by     TEXT REFERENCES decisions(id) ON DELETE SET NULL,
    tags              TEXT DEFAULT '[]',
    created_at        TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at        TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_decisions_project ON decisions(project_id, status);

CREATE TABLE IF NOT EXISTS memory_snapshots (
    id            TEXT PRIMARY KEY DEFAULT (hex(randomblob(16))),
    snapshot_id   TEXT NOT NULL,
    project_id    TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    category      TEXT NOT NULL,
    content       TEXT NOT NULL,
    importance    REAL NOT NULL,
    source        TEXT NOT NULL,
    tags          TEXT DEFAULT '[]',
    -- The ORIGINAL memory's timestamp, not the moment the snapshot was
    -- taken. A restore that reset created_at made an old memory look freshly
    -- written, and created_at feeds decay, so restoring "the same memory"
    -- silently aged it backwards.
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    -- Identity and history, so a restore returns the same memory rather than
    -- a lookalike. memory_embeddings and memory_links both reference
    -- memories(id) ON DELETE CASCADE, so a delete-then-reinsert destroys the
    -- embedding and the row's whole link graph even though the text comes
    -- back looking identical. NULL only on snapshots written before schema
    -- v13, which never recorded an id — those restore by content match
    -- instead.
    --
    -- pinned and resolved_at are deliberately not stored: the snapshot
    -- predicate is source != 'manual' AND pinned = 0 AND resolved_at IS
    -- NULL, so both are constant by construction. Restore re-checks them on
    -- the live row instead, which is the value that can actually change.
    memory_id     TEXT,
    access_count  INTEGER NOT NULL DEFAULT 0,
    last_accessed TEXT,
    agent         TEXT,
    session_id    TEXT,
    source_ref    TEXT,
    confidence    REAL,
    valid_from    TEXT,
    valid_until   TEXT,
    verified_at   TEXT,
    -- The scoped value of the memory itself, in memories.scope's JSON form.
    -- Without it a restore could not put scope back, which made the replace's
    -- dropped scope unrecoverable: the snapshot is the only undo history for
    -- a reflection replace (issue #572).
    scope         TEXT,
    -- Whether scope above is the memory's scope or merely the absence of
    -- one this build could record. migrateV14 adds both columns to a table
    -- whose earlier rows predate scope entirely, and for those rows scope IS
    -- NULL — the same value a genuinely unscoped v14 snapshot stores. Restore
    -- reads this flag instead of guessing from NULL: 1 means "trust the value,
    -- NULL included", 0 means "this snapshot cannot speak about scope" and the
    -- live row's own scope is left alone. Defaults to 0 so any row that did not
    -- state it — a legacy row, or an unverified hand backfill — is treated as
    -- unknown rather than as a claim that the memory had no scope.
    scope_captured INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_snapshots_project ON memory_snapshots(project_id, snapshot_id);

CREATE TABLE IF NOT EXISTS memory_links (
    source_id      TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
    target_id      TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
    relation       TEXT NOT NULL DEFAULT 'related'
                   CHECK (relation IN ('related', 'supersedes', 'contradicts', 'elaborates', 'causes', 'duplicate')),
    strength       REAL NOT NULL DEFAULT 0.5,
    source         TEXT NOT NULL DEFAULT 'auto'
                   CHECK (source IN ('auto', 'llm', 'manual')),
    created_at     TEXT NOT NULL DEFAULT (datetime('now')),
    invalidated_at TEXT,
    PRIMARY KEY (source_id, target_id, relation)
);
CREATE INDEX IF NOT EXISTS idx_links_source ON memory_links(source_id) WHERE invalidated_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_links_target ON memory_links(target_id) WHERE invalidated_at IS NULL;

CREATE TABLE IF NOT EXISTS supersede_checked (
    newer_id    TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
    older_id    TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    newer_hash  TEXT NOT NULL,
    older_hash  TEXT NOT NULL,
    checked_at  TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (newer_id, older_id)
);
CREATE INDEX IF NOT EXISTS idx_supersede_checked_project ON supersede_checked(project_id);

CREATE TABLE IF NOT EXISTS link_scans (
    memory_id  TEXT PRIMARY KEY REFERENCES memories(id) ON DELETE CASCADE,
    scanned_at TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Append-only history: one row per mutation of a memory (schema v17, issue
-- #578). Every writer appends a row in the SAME transaction as the mutation, so
-- history cannot diverge from state.
--
-- Each row is a version: the state the memory HELD once that write landed, not
-- the state it was about to have. The prior content of any write is therefore
-- the previous row's content, and a reader who wants "what did Ghost know at
-- time T" takes the newest row at or before T — a query the live memories table
-- cannot answer at all, because it keeps only the last value. Recording the
-- state AFTER the write is also the only shape an insert can have: an inserted
-- row has no prior state to record.
--
-- memory_id carries NO foreign key, deliberately. The one event whose value is
-- destroyed by the mutation it records is the delete — a hard DELETE takes the
-- row with it, so a cascading history table would be empty exactly when the
-- audit is asked for. Nothing reuses a memory id (ids come from
-- hex(randomblob(16)) and a snapshot restore reinstates the id it recorded) —
-- with ONE exception this table's own writers guard: a portable import carries
-- the artifact's id verbatim, so ImportMemory refuses an id that still has
-- history rather than splicing a deleted memory's record onto new text.
--
-- project_id DOES cascade: deleting a project is meant to take the whole corpus
-- with it. That makes this column load-bearing for every write that moves a
-- memory BETWEEN projects, which is why both of them have to reassign it here as
-- well as on memories: a merge and a promotion both KEEP the rows, so a history
-- row left naming the project the memory just left is taken by that project's
-- DELETE, leaving a live memory whose recorded past is gone. MergeProject (in
-- projectMergeStatements, and in the s.mergeProjectTx method the bind-recovery
-- paths call) and PromoteToGlobal both do this; a new writer that moves a memory
-- between projects has to as well.
--
-- The NAME is a distinction, not a description. This table is a change log: one
-- row per write, holding the state the memory had once that write landed. The
-- name memory_provenance went to a different and later concept -- EVIDENCE
-- records, several per memory (kind, agent, session_id, source_ref, confidence,
-- observed_at, verified_at) answering "who or what supports this memory", created
-- by migrateV18 below -- and this table answers a different question, which is
-- why it did not take that name. A schema name is permanent once released, and
-- the two concepts are easy to confuse in prose while being unrelated in fact.
CREATE TABLE IF NOT EXISTS memory_history (
    id          TEXT PRIMARY KEY DEFAULT (hex(randomblob(16))),
    memory_id   TEXT NOT NULL,
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    recorded_at TEXT NOT NULL DEFAULT (datetime('now')),
    -- Which write path touched the row: save (a new row), update (an edit),
    -- reflect (a consolidation rewrite, reuse or insert), merge (a near-
    -- duplicate fold strengthening an existing row), resolve / unresolve
    -- (resolved_at stamped or cleared), supersede / unsupersede (an active
    -- supersedes edge now points at this memory, or stopped), restore (a
    -- snapshot restore), import (a portable artifact), delete (the row is being
    -- removed). A CHECK rather than a convention: a phase no reader knows is a
    -- phase that can never be filtered, and a typo must not create a new one
    -- silently.
    phase       TEXT NOT NULL
                CHECK (phase IN (
                    'save', 'update', 'reflect', 'merge', 'resolve',
                    'unresolve', 'supersede', 'unsupersede', 'restore',
                    'import', 'delete', 'baseline'
                )),
    -- Who PERFORMED the write, when the write path knows: the save/merge/import
    -- paths carry a Provenance, the lifecycle passes (reflect, resolve,
    -- supersede) and Delete do not. NULL means the actor is not known, which is
    -- not a claim that nobody acted.
    agent       TEXT,
    session_id  TEXT,
    -- The other memory this event is about, when there is one. A delete row
    -- carries the id that replaced this row, which is the only way to follow one
    -- memory's history into its successor's after a consolidation's rewrite or
    -- merge gave the row a new id (#648). A supersede or unsupersede row carries
    -- the memory whose edge makes the claim — the one that replaced THIS row.
    -- NULL for a write that concerns one memory alone.
    related_id  TEXT,
    -- The incoming near-duplicate text a merge folded in, recorded only when the
    -- fold did not store it as its own row. The default fold keeps that text as
    -- a linked copy with its own save row, so this is NULL there; the FoldOnly
    -- path (a promotion into the global bucket) deliberately drops the wording,
    -- and without this column the only record of it was the database itself,
    -- before this table existed.
    merged_content TEXT,
    -- The state this memory held once the write landed. nullable because a
    -- delete row is written from the last live state and an older build could
    -- leave gaps, not because a memory has no state.
    content     TEXT,
    category    TEXT,
    importance  REAL,
    resolved_at TEXT,
    source      TEXT
);
-- One index, not two. The per-memory cap ranks by rowid (the implicit index,
-- free), MemoryHistory filters by memory_id, and an as_of read (#647) is
-- "the newest row for this memory at or before T" — which is served by this
-- index too, because memory_id is the leading column. A standalone recorded_at
-- index would have no reader, and every append would pay for a second b-tree
-- insert to keep it current: measured at a fifth of the cost of writing the
-- history row at all, on the write path's critical section.
CREATE INDEX IF NOT EXISTS idx_history_memory ON memory_history(memory_id, recorded_at);

-- Evidence records: SEVERAL rows per memory, each one an observation that
-- supports it (schema v18, issue #673). This is the table the name
-- memory_provenance was reserved for, and it is NOT memory_history: the change
-- log above answers "how did this row change", one row per write, and this one
-- answers "who or what supports this memory", one row per observation. A fact
-- claude-code reported in one session, codex reported in another, and a human
-- verified is three rows here and one row of mutable columns on memories, which
-- can only remember the last.
--
-- A fold is the case that makes the table worth having: a near-duplicate save
-- used to be a discard, so the second agent's report vanished with the incoming
-- wording. It now appends to the memory that SURVIVED.
--
-- The columns are the evidence itself, and every one of them is nullable except
-- kind: NULL means Ghost does not know, which is the truth, while "" would
-- claim a value that happens to be empty and a guessed agent or session would be
-- a provenance claim nobody made. observed_at is nullable for the same reason --
-- it is when Ghost recorded the observation, and a row migrated from a memory's
-- own columns cannot know that.
--
-- memory_id CASCADES, unlike memory_history's deliberately FK-free memory_id.
-- That asymmetry is the point of the two tables: the audit of a deletion must
-- outlive the row (so a hard DELETE leaves a tombstone), while evidence without
-- its memory means nothing -- "3 observations support this" is a claim about a
-- memory, and once the memory is gone the claim is about nothing. A purge
-- removes these rows explicitly as well, because PurgeMemoryHistory leaves the
-- memory in place and no cascade fires for a row that stays. And a consolidation
-- that REPLACES a memory takes its records with it, which is the cascade working
-- as specified: the evidence belonged to that row, and the replacement is a new
-- claim that needs its own observations.
CREATE TABLE IF NOT EXISTS memory_provenance (
    id          TEXT PRIMARY KEY DEFAULT (hex(randomblob(16))),
    memory_id   TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
    -- What kind of evidence this is. observed: a write path recorded what the
    -- host reported. imported: the fact arrived through a portable artifact.
    -- verified: somebody checked it (no writer yet -- the validity writers own
    -- that), so the kind is reserved rather than absent. legacy: migrateV18's
    -- seed of a memory that already held these values in its own columns.
    -- A CHECK rather than a convention, for the reason the history table's phase
    -- is one: a kind no reader knows is a kind no reader can filter.
    kind        TEXT NOT NULL
                CHECK (kind IN ('observed', 'imported', 'verified', 'legacy')),
    agent       TEXT,
    session_id  TEXT,
    source_ref  TEXT,
    confidence  REAL,
    observed_at TEXT,
    verified_at TEXT,
    -- The memory this record was INHERITED from, and empty for a record that
    -- observed this row directly.
    --
    -- A consolidation rewrite or merge mints a new id, and the foreign key below
    -- takes the source's evidence with it, so a carried row is a verbatim copy of
    -- its source's row and this is what says so. The copy is honest — the
    -- observation really was made, by that agent, at that time — but it is not an
    -- observation of THIS wording, and a reader has to be able to tell the
    -- difference between "an agent reported this" and "an agent reported something
    -- this was consolidated from". The kind does not change for the carry: the
    -- record is still the observation it was, and a fifth kind would lose which.
    --
    -- It is also the way back. The id it names is gone — that is why the record was
    -- carried — but the CHANGE LOG keeps that id's whole past, so a reader who
    -- wants to know what the source said follows this column into memory_history
    -- rather than into a dead end. The same pointer shape, pointing the other way.
    -- One index, on memory_id, and no index on carried_from: nothing looks a
    -- record up BY the memory it came from. The column is followed the other way
    -- -- the id it names is read out of the record and used against
    -- memory_history -- so a second b-tree would be an insert every consolidation
    -- pays and no query asks for. The same rule the change log's growth policy
    -- states for its own recorded_at.
    carried_from TEXT
);
CREATE INDEX IF NOT EXISTS idx_provenance_memory ON memory_provenance(memory_id);

-- The evidence a snapshot carries, so a restore brings the SUPPORT back with the
-- row. Same columns as memory_provenance, minus carried_from: a record restored
-- under its own id is its own record again, and marking it inherited would
-- misreport the row's own support as a successor's.
--
-- A table rather than a column on memory_snapshots because a memory can have
-- several records and a snapshot row is one row; and it is pruned with the
-- snapshots themselves (ReplaceNonManual's prune), so it cannot outlive the
-- snapshot it describes.
CREATE TABLE IF NOT EXISTS memory_snapshot_evidence (
    snapshot_id  TEXT NOT NULL,
    memory_id    TEXT NOT NULL,
    kind         TEXT NOT NULL
                 CHECK (kind IN ('observed', 'imported', 'verified', 'legacy')),
    agent        TEXT,
    session_id   TEXT,
    source_ref   TEXT,
    confidence   REAL,
    observed_at  TEXT,
    verified_at  TEXT
);
CREATE INDEX IF NOT EXISTS idx_snapshot_evidence ON memory_snapshot_evidence(snapshot_id, memory_id);

-- One row per retrieval call: what it retrieved and what it kept (#646). The
-- grain is the call, not the (call, memory) pair, because the audit's
-- denominator is calls — a table holding only the calls that admitted something
-- has already dropped the ones worth auditing.
--
-- No foreign key on project_id, deliberately, and for the reason memory_history
-- has none on its own: a purge of the PROJECT must be able to find these rows
-- with a plain predicate even after the memories are gone, and a project is not
-- deleted by a memory purge. The column is a name in a record, not a claim that
-- the memory table is the owner of it.
--
-- No text anywhere. query_hash is a sha256 digest or empty for a call that
-- carried no query (a session-start injection), and the CHECK is what makes that
-- structural rather than a convention: a column whose only accepted values are
-- 64 hex characters and the empty string cannot hold a question however a
-- future writer builds its statement.
CREATE TABLE IF NOT EXISTS retrieval_record (
    project_id TEXT NOT NULL,
    -- Empty over stdio, which reports no session. Load-bearing alongside
    -- source: on the transport Ghost ships, a session-start injection and a
    -- search are indistinguishable by session alone.
    session_id TEXT NOT NULL DEFAULT '',
    -- The surface the call came from (search, session_start, ...), so an audit
    -- can tell an injection from a search without re-reading the transcript.
    source     TEXT NOT NULL,
    query_hash TEXT NOT NULL DEFAULT ''
                CHECK (query_hash = '' OR
                       (length(query_hash) = 64 AND query_hash NOT GLOB '*[^0-9a-f]*')),
    -- The instant a historical read was assembled at, RFC 3339; empty for a
    -- current one, which is a fact and not an absent value.
    as_of   TEXT NOT NULL DEFAULT '',
    outcome TEXT NOT NULL,
    reason  TEXT NOT NULL DEFAULT '',
    -- The per-memory verdicts as a JSON array of {id, kept, stage, reason},
    -- because one call judges many rows and a row-per-pair table cannot also be
    -- the row that says the call happened.
    --
    -- Deliberately NOT CHECK(json_valid(verdicts)): the column's readers must
    -- tolerate a document they cannot read, and a constraint that refuses to
    -- store one would only mean the tolerance was unreachable from Ghost's own
    -- writers. Two different functions RAISE on a column they cannot read --
    -- json_each on a document that is not JSON, and the ->> path accessor on the
    -- value json_each yields from one that is an OBJECT rather than an array --
    -- so the ONE read that goes through either, the purge's predicate (a
    -- predicate must decide in SQL), goes through readableVerdicts, which closes
    -- both. RetrievalRecords reads the column raw on purpose and settles the
    -- same cases in one Go decode, which is cheaper than the two parses
    -- json_valid and json_type would each cost.
    verdicts    TEXT NOT NULL DEFAULT '[]',
    -- The STORE's clock, never the assembler's Now: the assembler binds Now for
    -- its own decisions and must not read a wall clock, and the instant a row
    -- became durable is the one that places it against a transcript.
    recorded_at TEXT NOT NULL DEFAULT (datetime('now'))
);
-- One b-tree, on project_id, for the per-project read the audit report needs
-- (#646 asks for "real precision per project"). HONEST ACCOUNTING: nothing in
-- this build reads it yet -- RetrievalRecords takes a limit and no project, and
-- the purge matches on a memory id -- so today this is an insert nobody uses.
-- It is kept rather than added later because the reader that uses it is the next
-- part and re-adding an index is itself a schema migration, and because the cost
-- is one b-tree insert per search inside a write that already costs 5000-row
-- cap's INSERT (measured ~45us total). A future reader that is NOT per project
-- should drop this, and the assertion that would catch a second index is the
-- stray-index count in TestMigrateFreshDBHasRetrievalRecord (migrate_test.go),
-- which is where the "one index, because every one is an insert inside the write
-- lock" rule is kept honest for this table.
-- recorded_at is deliberately NOT indexed, for the reason memory_history's is
-- not: it is second-precision, so a lookup by it is a range scan that can return
-- a window of rows the report cannot order within.
CREATE INDEX IF NOT EXISTS idx_retrieval_record_project ON retrieval_record(project_id);

CREATE TABLE IF NOT EXISTS maintenance_runs (
    id                   TEXT PRIMARY KEY DEFAULT (hex(randomblob(16))),
    kind                 TEXT NOT NULL,
    recorded_at          TEXT NOT NULL DEFAULT (datetime('now')),
    scratch_bytes        INTEGER NOT NULL DEFAULT 0,
    scratch_reaped_bytes INTEGER NOT NULL DEFAULT 0,
    scratch_reaped_count INTEGER NOT NULL DEFAULT 0,
    note                 TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_maintenance_runs_at ON maintenance_runs(recorded_at DESC);
`

// readOnlyDSN builds the read-only DSN OpenReadDB opens. The file: URI
// form is required — modernc.org/sqlite honors mode=ro only on URI DSNs, and
// a bare path opens read-write and would create a phantom empty ghost.db on
// first read. The path is URI-escaped so a '?' or '#' in it cannot corrupt the
// query, and no journal_mode pragma is set (a read-only connection cannot
// write the header). WAL is persisted in the database file itself rather than
// negotiated per connection, so this connection is in WAL mode too.
//
// Deliberately unexported: mcpinit builds the same URI for its own read-only
// write-adjacent paths. This one exists for OpenReadDB.
func readOnlyDSN(dbPath string) string {
	u := url.URL{
		Scheme:   "file",
		Opaque:   (&url.URL{Path: dbPath}).EscapedPath(),
		RawQuery: "mode=ro&_pragma=busy_timeout(1000)",
	}
	return u.String()
}

// ErrInMemoryReadHandle means a read-only handle was asked for a private
// in-memory database. ":memory:" is per-connection: a second connection opens
// a second, empty database, so a read handle built on it would report an empty
// store rather than the one the caller seeded. Callers holding only an
// in-memory handle keep using it, and their snapshot reads run on it directly.
var ErrInMemoryReadHandle = errors.New("read-only handle cannot open an in-memory database")

// OpenReadDB opens an EXISTING database for reading only. It runs no DDL
// and no migrations, and it refuses a path that does not exist rather than
// creating one. It is the single read-only constructor: a Store that takes
// snapshot reads is given one of these through NewStoreWithRead, and a CLI
// diagnostic opens it directly.
//
// A diagnostic command must be able to report "there is no database" without
// making that statement untrue: OpenDB would create the file, stamp
// user_version and seed the builtin global rows, so the first status run on a
// fresh install would turn the next one's missing-database line into a healthy
// one. Callers stat first, or treat ErrNoDatabase as "nothing to report".
func OpenReadDB(dbPath string) (*sql.DB, error) {
	if dbPath == ":memory:" {
		return nil, ErrInMemoryReadHandle
	}
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNoDatabase, dbPath)
		}
		return nil, fmt.Errorf("stat database: %w", err)
	}
	db, err := sql.Open("sqlite", readOnlyDSN(dbPath))
	if err != nil {
		return nil, fmt.Errorf("open database read-only: %w", err)
	}
	db.SetMaxOpenConns(1) // matches OpenDB; SQLite is single-writer
	return db, nil
}

// ErrNoDatabase means the database file does not exist yet. A read-only caller
// distinguishes it from other open failures because the correct response is to
// report nothing, not to retry or to create one.
var ErrNoDatabase = errors.New("no Ghost database")

// OpenDB opens or creates the SQLite database and runs migrations.
func OpenDB(dbPath string) (*sql.DB, error) {
	// File paths go through the file: URI form with the path percent-encoded —
	// a '?' or '#' in the data-dir path (legal in $XDG_DATA_HOME/$HOME) would
	// otherwise be parsed as the query/fragment separator, silently opening a
	// truncated path. ":memory:" stays bare: the file::memory: URI form has
	// different sharing semantics.
	dsn := dbPath
	if dbPath != ":memory:" {
		u := url.URL{Scheme: "file", Opaque: (&url.URL{Path: dbPath}).EscapedPath()}
		dsn = u.String()
	}
	// _txlock=immediate makes BeginTx issue BEGIN IMMEDIATE, taking the write
	// lock when the transaction starts rather than on its first write
	// statement. The default (deferred) is only safe when the first statement
	// is already a write: a transaction that reads first holds a WAL read
	// snapshot, and if another process commits before that transaction's
	// first write, the read-to-write upgrade fails with SQLITE_BUSY_SNAPSHOT
	// (517) — which busy_timeout does NOT retry, because it is a snapshot
	// conflict rather than a lock conflict. UpdateMemory is the case that
	// exposed this: reads the row, then UPDATEs it, and under concurrent
	// handles every one of those updates failed in well under a millisecond,
	// far faster than any timeout could have been reached.
	//
	// Every other BeginTx here is write-first, so immediate changes nothing
	// for them — they acquire the same lock on their first statement anyway.
	db, err := sql.Open("sqlite", dsn+"?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	db.SetMaxOpenConns(1) // SQLite is single-writer; prevent connection pool contention

	// Distinguish a fresh database from an existing one BEFORE initSQL runs:
	// fresh databases get the current schema from initSQL and are stamped at
	// schemaVersion directly; existing ones must pass through migrate(), since
	// CREATE TABLE IF NOT EXISTS never alters tables that already exist.
	var tableCount int
	if err := db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table'`,
	).Scan(&tableCount); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("inspect schema: %w", err)
	}

	// The database file and the -wal and -shm files SQLite maintains beside it
	// all exist now, so their modes are the ones that will be on disk from
	// here on. This is the one place any ghost opens the database read-write,
	// so it is the one place that can guarantee the modes. It cannot fail the
	// open, and it does not need the schema to be current to be correct.
	TightenPermissions(dbPath)

	// Read the stamped version and refuse a newer database BEFORE running any
	// DDL (issue #560). initSQL is CREATE ... IF NOT EXISTS, so on a database
	// that merely has more than this build knows it is nearly a no-op — which
	// is what hid the ordering. It is not one for every difference: an object
	// the newer build renamed, replaced or dropped (a trigger, an index) is
	// recreated here, in a database this call then declares unreadable. A
	// refusal that has already written is not a refusal.
	//
	// A fresh database has no stamp to read, and gets the current schema plus
	// the stamp below; there is nothing above schemaVersion to refuse.
	var version int
	if tableCount > 0 {
		if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("read schema version: %w", err)
		}
		// A database written by a newer ghost has columns, constraints, or tables
		// this binary does not know about. Opening it read-write would let this
		// binary corrupt state it cannot interpret, so refuse instead.
		if version > schemaVersion {
			_ = db.Close()
			return nil, fmt.Errorf("database schema v%d is newer than this ghost build (v%d) — upgrade ghost before opening it", version, schemaVersion)
		}
	}

	// A same-named table that is not Ghost's, refused BEFORE initSQL rather than
	// beside the evidence check below. The difference is which error the operator
	// gets: initSQL's own `CREATE INDEX ... ON retrieval_record(project_id)` is
	// the statement that fails against a foreign table, and it fails with "no such
	// column: project_id" — which names neither the table nor the way out. The
	// evidence check sits below because initSQL's index on it cannot fail against
	// the #664 shape (that table has memory_id, which is all it names); this index
	// names a column, so the check has to come first.
	//
	// It runs before the DDL and therefore also before backupBeforeMigrate, for
	// the reason the branch below spells out: the condition is permanent, the step
	// rolls back, so every later open re-enters — and a refusal that has already
	// written a copy is not the refusal we want.
	if err := refuseForeignRetrievalRecordTable(db); err != nil {
		_ = db.Close()
		return nil, err
	}

	if _, err := db.Exec(initSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}

	if tableCount == 0 {
		if err := ensurePostMigrationIndexes(db); err != nil {
			_ = db.Close()
			return nil, err
		}
		if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("stamp schema version: %w", err)
		}
		return db, nil
	}

	if version < schemaVersion {
		// A schema-shape refusal, BEFORE the backup (#673's memory_provenance
		// check, which is the one today). The ordering is the whole point: the
		// condition is permanent — nothing converts a foreign table, the operator
		// drops it — and it leaves user_version where it was, so every later open
		// re-enters this branch. Refusing after the backup would write a full copy
		// of the database on every open of a store that cannot be opened, and two
		// opens in the same wall-clock second collide on the copy's name, so the
		// retry an operator is certain to make reports "refusing to overwrite an
		// existing file" and names neither the table nor the remedy. A refusal
		// that has already written a copy is not the refusal we want (#560's rule,
		// applied one level down).
		if err := refuseForeignProvenanceTable(db); err != nil {
			_ = db.Close()
			return nil, err
		}
		// The retrieval record's shape refusal has already run above, before
		// initSQL — see the note there on why it cannot be checked from here.
		// Migration steps rebuild and DROP tables, so a bug in a step is
		// unrecoverable without a copy. Fail closed: if the backup cannot be
		// written, do not migrate.
		fresh, err := backupBeforeMigrate(db, dbPath)
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("pre-migration backup: %w", err)
		}
		// The copy landed, so the safety net for THIS upgrade exists and the
		// older ones can stop accumulating (#542: a directory that has seen a
		// dozen upgrades held a dozen full copies of the database). Only now,
		// and only on this path — an open with nothing to migrate writes no
		// backup and prunes nothing.
		//
		// The path is passed in rather than re-derived from the clock, and
		// excluded from removal outright: the directory can hold backups
		// stamped further ahead than this machine's own clock (a data dir
		// restored from one that ran ahead, a corrected clock, a hand-seeded
		// file), and deciding "the newest is mine" from the stamp could name
		// the wrong file — deleting the copy this migrate is about to need.
		prunePreMigrateBackups(dbPath, fresh)
		if err := migrate(db, version); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("migrate schema v%d→v%d: %w", version, schemaVersion, err)
		}
	}
	if err := ensurePostMigrationIndexes(db); err != nil {
		_ = db.Close()
		return nil, err
	}

	return db, nil
}

// ensurePostMigrationIndexes creates the indexes whose columns a migration added,
// which is why they are not in initSQL.
//
// initSQL is CREATE ... IF NOT EXISTS, so on a database that predates a column
// every statement in it is a no-op — EXCEPT one that reads the missing column by
// name. A partial index is exactly that statement: the repository-identity index
// on projects.repo_remote, and the session-expiry index below on
// memories.retention/expires_at. Running either against a store that has not
// been migrated yet fails the whole open with SQLite's "no such column", which
// is the shape #560 is about — a DDL that cannot be delivered to an existing
// database run before the migration that delivers it.
//
// So they are created here, after migrate(), on the fresh path and the upgraded
// path alike, and by one function so a third index cannot be added to one branch
// only. Both are partial indexes over the rows their predicate selects, so the
// write cost falls only on those rows: the repository one only on projects with
// a remote, the session one only on session-tier memories, which is what makes
// the second one free for the 99% of a corpus that is not session-scoped.
func ensurePostMigrationIndexes(db *sql.DB) error {
	for _, stmt := range []struct{ name, ddl string }{
		{"repository identity index", `
			CREATE UNIQUE INDEX IF NOT EXISTS idx_projects_repo_remote
			ON projects(repo_remote) WHERE repo_remote IS NOT NULL AND repo_remote <> ''
		`},
		// Prune's only index, and a partial one for the reason above: it describes
		// session rows alone, so every write to a project or persistent row costs
		// it nothing, and the read it serves is a maintenance pass over a corpus
		// that may hold tens of thousands of rows of which a handful are
		// candidates. idx_memories_project_cat does not help -- the predicate is
		// on the tier and the expiry, neither of which it leads with.
		{"session-expiry index", `
			CREATE INDEX IF NOT EXISTS idx_memories_session_expiry
			ON memories(expires_at) WHERE retention = 'session'
		`},
	} {
		if _, err := db.Exec(stmt.ddl); err != nil {
			return fmt.Errorf("create %s: %w", stmt.name, err)
		}
	}
	return nil
}

// backupBeforeMigrate writes a one-shot copy of dbPath beside it before any
// migration step runs, named "<db>.pre-migrate-<unix>", and returns the path it
// wrote so the caller can name it later — deciding which files are deletable by
// re-reading the clock would race the clock this name came from. In-memory
// databases are skipped and return an empty path. A failure aborts the open
// rather than proceeding without a fallback — the caller decides whether an
// un-migratable database is acceptable, and the only alternative is an
// unrecoverable destructive migration.
//
// The copy itself is vacuumInto, the same VACUUM INTO that backs `ghost backup`:
// one implementation of "put a restorable copy of this database over there", so
// the refusal to replace an existing file, the mode the copy is created at, and
// the permission pass cannot drift apart between the two callers. The path comes
// back from here because the caller has to name this specific file as the one
// prunePreMigrateBackups must not remove.
func backupBeforeMigrate(db *sql.DB, dbPath string) (string, error) {
	if dbPath == ":memory:" {
		return "", nil
	}
	backup := fmt.Sprintf("%s.pre-migrate-%d", dbPath, time.Now().Unix())
	// vacuumInto creates the copy at 0600 before SQLite writes a byte of it, and
	// tightens it afterwards — the same pass the open path runs on the live
	// files, whose comment explains why a copy needs it at all: a full copy of the
	// memory database is shielded only by the 0700 data directory, and a database
	// opened outside it (eval, bench) has no such shield. A chmod failure is
	// reported, not fatal: the migration's safety net exists either way.
	if err := vacuumInto(context.Background(), db, backup); err != nil {
		return "", err
	}
	return backup, nil
}

// preMigrateBackupKeep is how many pre-migration copies of one database stay in
// the data directory: the one just written plus the two it supersedes. Each is
// a full copy of the database, so a set that nothing ever culls grows with
// every upgrade (#542) while its only reader is the human deciding which
// upgrade to roll back to — three is more hindsight than that needs.
const preMigrateBackupKeep = 3

// prunePreMigrateBackups keeps the newest preMigrateBackupKeep regular files
// named "<db>.pre-migrate-<unix>" beside dbPath and removes the older ones,
// where fresh — the path backupBeforeMigrate just wrote, or "" when there is
// none — is always one of the kept slots. The prefix is built from the path
// rather than a constant so a database called anything but ghost.db only ever
// prunes its own copies.
//
// fresh is passed in rather than inferred. The directory can hold backups
// stamped further ahead than this machine's clock — a data dir restored from a
// machine whose clock ran ahead, a corrected clock, a file a user renamed — and
// an implementation that picked "the newest by stamp" and kept that would
// treat a stranger's timestamp as proof that the copy this migrate is about to
// need is expendable. It is excluded by name before any ordering happens.
//
// The match is deliberately narrow: the suffix must be a bare unix timestamp
// (numeric order, not the lexicographic order a glob would impose), the entry
// must be a regular file as reported by Lstat — a symlink wearing the name is
// left as the link it is, never renamed, never followed — and every other file
// in the directory (hand-made ghost.db.backup-*, ghost.db.pre-<version>-*, the
// database and its sidecars) is outside the match entirely.
//
// Best effort: a directory that cannot be read or a file that cannot be removed
// is a warning, never an error. Losing a cleanup pass must not stand between a
// migration and the backup it just wrote.
func prunePreMigrateBackups(dbPath, fresh string) {
	if dbPath == ":memory:" {
		// Dir(":memory:") would name the working directory, which holds
		// nothing of this database's and is not ours to walk.
		return
	}
	dir := filepath.Dir(dbPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		slog.Warn("could not list directory to prune pre-migration backups", "dir", dir, "error", err)
		return
	}

	freshName := ""
	if fresh != "" {
		freshName = filepath.Base(fresh)
	}
	prefix := filepath.Base(dbPath) + ".pre-migrate-"
	type candidate struct {
		path  string
		stamp int64
	}
	var found []candidate
	for _, e := range entries {
		name := e.Name()
		if name == freshName {
			// The copy this open depends on. Removed from the pool outright
			// rather than sorted to the top, so no ordering of the remaining
			// stamps can ever reach it.
			continue
		}
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		suffix := name[len(prefix):]
		stamp, convErr := strconv.ParseInt(suffix, 10, 64)
		// ParseInt accepts "+7"; FormatInt never produces it, and a negative
		// stamp is not a timestamp. Both would widen the match past what
		// backupBeforeMigrate can write.
		if convErr != nil || stamp < 0 || strconv.FormatInt(stamp, 10) != suffix {
			continue
		}
		full := filepath.Join(dir, name)
		fi, statErr := os.Lstat(full)
		if statErr != nil || !fi.Mode().IsRegular() {
			// Symlink, directory, or a file that vanished between ReadDir
			// and here: not a backup this function wrote, so not one it may
			// remove.
			continue
		}
		found = append(found, candidate{path: full, stamp: stamp})
	}

	// One slot of the budget is already spent on the copy just written, so the
	// rest get preMigrateBackupKeep-1 between them.
	keep := preMigrateBackupKeep
	if freshName != "" {
		keep--
	}
	if len(found) <= keep {
		return
	}
	// Newest first by the numeric suffix. Sorting the names as strings would
	// rank "999" above "1003" and prune the wrong end.
	sort.Slice(found, func(i, j int) bool { return found[i].stamp > found[j].stamp })
	for _, c := range found[keep:] {
		if err := os.Remove(c.path); err != nil {
			slog.Warn("could not prune old pre-migration backup", "path", c.path, "error", err)
		}
	}
}
