package memory

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// legacySQL approximates a pre-versioning database: memories.source CHECK
// without 'onboarding'/'decision_log', memory_snapshots without its FK, and
// the pre-v0.8.0 assistant-era tables still present.
const legacySQL = `
CREATE TABLE projects (
    id          TEXT PRIMARY KEY,
    path        TEXT NOT NULL UNIQUE,
    name        TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE memories (
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
                  CHECK (source IN ('reflection', 'chat', 'manual', 'tool', 'mcp')),
    tags          TEXT DEFAULT '[]',
    pinned        INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE VIRTUAL TABLE memories_fts USING fts5(
    content,
    content=memories,
    content_rowid=rowid,
    tokenize='porter unicode61'
);

CREATE TRIGGER memories_ai AFTER INSERT ON memories BEGIN
    INSERT INTO memories_fts(rowid, content) VALUES (new.rowid, new.content);
END;

CREATE TRIGGER memories_ad AFTER DELETE ON memories BEGIN
    INSERT INTO memories_fts(memories_fts, rowid, content) VALUES('delete', old.rowid, old.content);
END;

CREATE TRIGGER memories_au AFTER UPDATE ON memories BEGIN
    INSERT INTO memories_fts(memories_fts, rowid, content) VALUES('delete', old.rowid, old.content);
    INSERT INTO memories_fts(rowid, content) VALUES (new.rowid, new.content);
END;

CREATE TABLE memory_snapshots (
    id            TEXT PRIMARY KEY DEFAULT (hex(randomblob(16))),
    snapshot_id   TEXT NOT NULL,
    project_id    TEXT NOT NULL,
    category      TEXT NOT NULL,
    content       TEXT NOT NULL,
    importance    REAL NOT NULL,
    source        TEXT NOT NULL,
    tags          TEXT DEFAULT '[]',
    created_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE notifications (id INTEGER PRIMARY KEY, body TEXT);
CREATE TABLE reminders (id INTEGER PRIMARY KEY, body TEXT);
CREATE TABLE scheduled_jobs (id INTEGER PRIMARY KEY, body TEXT);
`

// newLegacyDB writes a legacy-schema database with sample rows and returns its path.
func newLegacyDB(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	defer db.Close() //nolint:errcheck
	if _, err := db.Exec(legacySQL); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	seed := []string{
		`INSERT INTO projects (id, path, name) VALUES ('p1', '/tmp/legacy-p1', 'p1')`,
		`INSERT INTO memories (id, project_id, category, content, source) VALUES
			('m1', 'p1', 'fact', 'obsidian vault mirror is one-way', 'mcp'),
			('m2', 'p1', 'gotcha', 'fts rebuild preserves rowids', 'reflection')`,
		`INSERT INTO memory_snapshots (id, snapshot_id, project_id, category, content, importance, source)
			VALUES ('s1', 'snap1', 'p1', 'fact', 'snapshot row', 0.5, 'mcp')`,
		`INSERT INTO notifications (body) VALUES ('stale')`,
	}
	for _, s := range seed {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed legacy db: %v", err)
		}
	}
	return dbPath
}

func schemaVersionOf(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	return v
}

// TestMigrateLegacyDB: opening a legacy database must rebuild memories with the
// widened source CHECK, add the memory_snapshots FK, drop assistant-era tables,
// keep all rows and FTS integrity, and stamp user_version — all in one OpenDB.
func TestMigrateLegacyDB(t *testing.T) {
	dbPath := newLegacyDB(t)

	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB on legacy db: %v", err)
	}
	defer db.Close() //nolint:errcheck

	if v := schemaVersionOf(t, db); v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}

	// Rows survived the rebuild.
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM memories`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("memories after migration: n=%d err=%v, want 2", n, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM memory_snapshots`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("memory_snapshots after migration: n=%d err=%v, want 1", n, err)
	}

	// The widened CHECK accepts the sources that used to fail.
	for _, src := range []string{"onboarding", "decision_log"} {
		if _, err := db.Exec(
			`INSERT INTO memories (id, project_id, content, source) VALUES (?, 'p1', 'probe', ?)`,
			"probe_"+src, src,
		); err != nil {
			t.Errorf("insert with source=%s still fails: %v", src, err)
		}
	}

	// FTS was rebuilt and still matches pre-migration content.
	if err := db.QueryRow(
		`SELECT count(*) FROM memories_fts WHERE memories_fts MATCH 'obsidian'`,
	).Scan(&n); err != nil || n != 1 {
		t.Errorf("fts match after migration: n=%d err=%v, want 1", n, err)
	}
	// ...and the recreated triggers index new rows.
	if _, err := db.Exec(
		`INSERT INTO memories (id, project_id, content, source) VALUES ('m3', 'p1', 'zanzibar trigger probe', 'mcp')`,
	); err != nil {
		t.Fatalf("insert after migration: %v", err)
	}
	if err := db.QueryRow(
		`SELECT count(*) FROM memories_fts WHERE memories_fts MATCH 'zanzibar'`,
	).Scan(&n); err != nil || n != 1 {
		t.Errorf("fts trigger after migration: n=%d err=%v, want 1", n, err)
	}

	// The snapshots FK cascades on project delete.
	if _, err := db.Exec(`DELETE FROM projects WHERE id = 'p1'`); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM memory_snapshots`).Scan(&n); err != nil || n != 0 {
		t.Errorf("memory_snapshots after project delete: n=%d err=%v, want 0 (FK cascade)", n, err)
	}

	// Assistant-era tables are gone.
	for _, table := range []string{"notifications", "reminders", "scheduled_jobs"} {
		var c int
		err := db.QueryRow(
			`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&c)
		if err != nil || c != 0 {
			t.Errorf("legacy table %s still present (c=%d err=%v)", table, c, err)
		}
	}
}

// TestMigrateFreshDBStamped: a brand-new database gets the current schema from
// initSQL and is stamped at schemaVersion without running any migration.
func TestMigrateFreshDBStamped(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close() //nolint:errcheck
	if v := schemaVersionOf(t, db); v != schemaVersion {
		t.Errorf("fresh db user_version = %d, want %d", v, schemaVersion)
	}
}

// TestMigrateHandMigratedDB: a database whose tables already match the current
// schema but whose user_version is still 0 (hand-migrated) must be stamped
// without a rebuild — the introspection guards skip work that is already done.
func TestMigrateHandMigratedDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")

	// Create a current-schema database, then reset its version stamp to 0.
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO projects (id, path, name) VALUES ('p1', '/tmp/hand-p1', 'p1')`,
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO memories (id, project_id, content, source) VALUES ('m1', 'p1', 'kept', 'decision_log')`,
	); err != nil {
		t.Fatalf("seed memory: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 0`); err != nil {
		t.Fatalf("reset version: %v", err)
	}
	_ = db.Close()

	db, err = OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB (second): %v", err)
	}
	defer db.Close() //nolint:errcheck

	if v := schemaVersionOf(t, db); v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}
	var content string
	if err := db.QueryRow(`SELECT content FROM memories WHERE id = 'm1'`).Scan(&content); err != nil || content != "kept" {
		t.Errorf("memory lost on stamp-only migration: %q %v", content, err)
	}
}

// TestMigrateIdempotent: opening an already-migrated database repeatedly is a no-op.
func TestMigrateIdempotent(t *testing.T) {
	dbPath := newLegacyDB(t)
	for i := 0; i < 3; i++ {
		db, err := OpenDB(dbPath)
		if err != nil {
			t.Fatalf("OpenDB round %d: %v", i, err)
		}
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM memories`).Scan(&n); err != nil || n != 2 {
			t.Fatalf("round %d: memories n=%d err=%v, want 2", i, n, err)
		}
		_ = db.Close()
	}
	// The file on disk holds the stamp (not just the connection).
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	defer db.Close() //nolint:errcheck
	if v := schemaVersionOf(t, db); v != schemaVersion {
		t.Errorf("persisted user_version = %d, want %d", v, schemaVersion)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("db missing: %v", err)
	}
}
