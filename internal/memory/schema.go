package memory

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
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
    scope         TEXT
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

// readOnlyDSN builds the read-only DSN OpenDBReadOnly opens. The file: URI
// form is required — modernc.org/sqlite honors mode=ro only on URI DSNs, and
// a bare path opens read-write and would create a phantom empty ghost.db on
// first read. The path is URI-escaped so a '?' or '#' in it cannot corrupt the
// query, and no journal_mode pragma is set (a read-only connection cannot
// write the header). WAL is persisted in the database file itself rather than
// negotiated per connection, so this connection is in WAL mode too.
//
// Deliberately unexported: mcpinit and cmd/ghost each already build this same
// URI for their own read-only paths, and consolidating them is a separate
// change with its own blast radius. This one exists for OpenDBReadOnly and
// does not claim to be the only spelling in the tree.
func readOnlyDSN(dbPath string) string {
	u := url.URL{
		Scheme:   "file",
		Opaque:   (&url.URL{Path: dbPath}).EscapedPath(),
		RawQuery: "mode=ro&_pragma=busy_timeout(1000)",
	}
	return u.String()
}

// OpenDBReadOnly opens an EXISTING database for reading only. It runs no DDL
// and no migrations, and it refuses a path that does not exist rather than
// creating one.
//
// A diagnostic command must be able to report "there is no database" without
// making that statement untrue: OpenDB would create the file, stamp
// user_version and seed the builtin global rows, so the first status run on a
// fresh install would turn the next one's missing-database line into a healthy
// one. Callers stat first, or treat ErrNoDatabase as "nothing to report".
func OpenDBReadOnly(dbPath string) (*sql.DB, error) {
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

	if _, err := db.Exec(initSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}

	if tableCount == 0 {
		if _, err := db.Exec(`
			CREATE UNIQUE INDEX IF NOT EXISTS idx_projects_repo_remote
			ON projects(repo_remote) WHERE repo_remote IS NOT NULL AND repo_remote <> ''
		`); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("create repository identity index: %w", err)
		}
		if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("stamp schema version: %w", err)
		}
		return db, nil
	}

	var version int
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
	if version < schemaVersion {
		// Migration steps rebuild and DROP tables, so a bug in a step is
		// unrecoverable without a copy. Fail closed: if the backup cannot be
		// written, do not migrate.
		if err := backupBeforeMigrate(db, dbPath); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("pre-migration backup: %w", err)
		}
		if err := migrate(db, version); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("migrate schema v%d→v%d: %w", version, schemaVersion, err)
		}
	}
	if _, err := db.Exec(`
		CREATE UNIQUE INDEX IF NOT EXISTS idx_projects_repo_remote
		ON projects(repo_remote) WHERE repo_remote IS NOT NULL AND repo_remote <> ''
	`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ensure repository identity index: %w", err)
	}

	return db, nil
}

// backupBeforeMigrate writes a one-shot copy of dbPath beside it before any
// migration step runs, named "<db>.pre-migrate-<unix>". In-memory databases are
// skipped. A failure aborts the open rather than proceeding without a fallback
// — the caller decides whether an un-migratable database is acceptable, and the
// only alternative is an unrecoverable destructive migration.
func backupBeforeMigrate(db *sql.DB, dbPath string) error {
	if dbPath == ":memory:" {
		return nil
	}
	backup := fmt.Sprintf("%s.pre-migrate-%d", dbPath, time.Now().Unix())
	if _, err := os.Stat(backup); err == nil {
		return fmt.Errorf("backup path already exists: %s", backup)
	}
	if _, err := db.Exec(`VACUUM INTO ?`, backup); err != nil {
		return fmt.Errorf("vacuum into %s: %w", backup, err)
	}
	// VACUUM INTO names no mode for the file it creates, so the copy lands at
	// whatever SQLite's default is minus the umask — a full copy of the memory
	// database, at the same width this open is in the middle of removing. It is
	// only shielded by the 0700 data directory, and a database opened outside
	// that directory (eval, bench) has no such shield, so tighten the copy here
	// rather than leave it to a pass that walks only the three live files. A
	// chmod failure is reported, not fatal: the migration's safety net exists
	// either way.
	TightenPermissions(backup)
	return nil
}
