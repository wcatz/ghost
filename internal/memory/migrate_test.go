package memory

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
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

	// migrateV1's rebuild is frozen in time and reinstalls the old unguarded
	// memories_au body; migrateV4 must re-narrow it afterward so a legacy DB
	// that runs the full V1->V4 chain ends up guarded too, not just DBs that
	// start at v3.
	var auSQL string
	if err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='trigger' AND name='memories_au'`,
	).Scan(&auSQL); err != nil {
		t.Fatalf("read memories_au trigger SQL: %v", err)
	}
	if !strings.Contains(auSQL, "old.content != new.content") {
		t.Errorf("memories_au trigger missing content-change guard after full migration chain: %s", auSQL)
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

// TestMigrateFreshDBHasResolvedAt: a brand-new database (initSQL path, no
// legacy schema involved) must have the resolved_at column from the start —
// guards against resolved_at silently going missing from initSQL while
// migrateV2 still exists to paper over it on upgraded databases.
func TestMigrateFreshDBHasResolvedAt(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close() //nolint:errcheck

	if _, err := db.Exec(`SELECT resolved_at FROM memories LIMIT 0`); err != nil {
		t.Errorf("resolved_at column missing on fresh db: %v", err)
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

// TestMigrateAddsResolvedAt: a pre-versioning database gains the resolved_at
// column, keeps all rows and FTS integrity, and lands at the current version.
func TestMigrateAddsResolvedAt(t *testing.T) {
	dbPath := newLegacyDB(t)

	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB on legacy db: %v", err)
	}
	defer db.Close() //nolint:errcheck

	if v := schemaVersionOf(t, db); v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}

	// The column exists and defaults to NULL (active) for migrated rows.
	var nNull int
	if err := db.QueryRow(
		`SELECT count(*) FROM memories WHERE resolved_at IS NULL`,
	).Scan(&nNull); err != nil || nNull != 2 {
		t.Fatalf("resolved_at NULL count: n=%d err=%v, want 2", nNull, err)
	}

	// A value can be written and read back.
	if _, err := db.Exec(
		`UPDATE memories SET resolved_at = datetime('now') WHERE id = 'm1'`,
	); err != nil {
		t.Fatalf("write resolved_at: %v", err)
	}
	var nResolved int
	if err := db.QueryRow(
		`SELECT count(*) FROM memories WHERE resolved_at IS NOT NULL`,
	).Scan(&nResolved); err != nil || nResolved != 1 {
		t.Errorf("resolved_at set count: n=%d err=%v, want 1", nResolved, err)
	}

	// FTS survived the column add (external-content index keyed on rowid).
	var nFts int
	if err := db.QueryRow(
		`SELECT count(*) FROM memories_fts WHERE memories_fts MATCH 'obsidian'`,
	).Scan(&nFts); err != nil || nFts != 1 {
		t.Errorf("fts match after add-column migration: n=%d err=%v, want 1", nFts, err)
	}
}

// v2WithLinksSQL is a schemaVersion-2 database: memories, memory_links (old
// CHECK — no 'duplicate'), and the surrounding tables current initSQL creates.
// Distinct from legacySQL, which predates memory_links entirely.
const v2WithLinksSQL = `
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
    category      TEXT NOT NULL DEFAULT 'fact',
    content       TEXT NOT NULL,
    importance    REAL NOT NULL DEFAULT 0.5,
    access_count  INTEGER NOT NULL DEFAULT 0,
    last_accessed TEXT,
    source        TEXT NOT NULL DEFAULT 'reflection',
    tags          TEXT DEFAULT '[]',
    pinned        INTEGER NOT NULL DEFAULT 0,
    resolved_at   TEXT,
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

CREATE TABLE memory_links (
    source_id      TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
    target_id      TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
    relation       TEXT NOT NULL DEFAULT 'related'
                   CHECK (relation IN ('related', 'supersedes', 'contradicts', 'elaborates', 'causes')),
    strength       REAL NOT NULL DEFAULT 0.5,
    source         TEXT NOT NULL DEFAULT 'auto'
                   CHECK (source IN ('auto', 'llm', 'manual')),
    created_at     TEXT NOT NULL DEFAULT (datetime('now')),
    invalidated_at TEXT,
    PRIMARY KEY (source_id, target_id, relation)
);
`

// newV2WithLinksDB writes a schemaVersion-2 database with two memories and a
// 'related' link between them, stamped at user_version=2, and returns its path.
func newV2WithLinksDB(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open v2 db: %v", err)
	}
	defer db.Close() //nolint:errcheck
	if _, err := db.Exec(v2WithLinksSQL); err != nil {
		t.Fatalf("create v2 schema: %v", err)
	}
	seed := []string{
		`INSERT INTO projects (id, path, name) VALUES ('p1', '/tmp/v2-p1', 'p1')`,
		`INSERT INTO memories (id, project_id, category, content, source) VALUES
			('m1', 'p1', 'fact', 'existing memory one', 'mcp'),
			('m2', 'p1', 'fact', 'existing memory two', 'mcp')`,
		`INSERT INTO memory_links (source_id, target_id, relation, strength, source)
			VALUES ('m1', 'm2', 'related', 0.6, 'auto')`,
		`PRAGMA user_version = 2`,
	}
	for _, s := range seed {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed v2 db: %v", err)
		}
	}
	return dbPath
}

// TestMigrateV3AddsDuplicateRelation: a schemaVersion-2 database with an
// existing memory_links row must upgrade to schemaVersion 3, keep the
// existing link intact, and accept a 'duplicate' relation afterward.
func TestMigrateV3AddsDuplicateRelation(t *testing.T) {
	dbPath := newV2WithLinksDB(t)

	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB on v2-with-links db: %v", err)
	}
	defer db.Close() //nolint:errcheck

	if v := schemaVersionOf(t, db); v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}

	// The existing 'related' link survived the rebuild.
	var strength float64
	err = db.QueryRow(
		`SELECT strength FROM memory_links WHERE source_id = 'm1' AND target_id = 'm2' AND relation = 'related'`,
	).Scan(&strength)
	if err != nil {
		t.Fatalf("existing link missing after migration: %v", err)
	}
	if strength != 0.6 {
		t.Errorf("existing link strength = %f, want 0.6", strength)
	}

	// The widened CHECK now accepts 'duplicate'.
	if _, err := db.Exec(
		`INSERT INTO memory_links (source_id, target_id, relation, strength, source) VALUES ('m2', 'm1', 'duplicate', 0.8, 'auto')`,
	); err != nil {
		t.Errorf("insert with relation='duplicate' still fails: %v", err)
	}
}

// v3SQL is a schemaVersion-3 database: current-shape memories/memories_fts
// and all three FTS triggers, but memories_au still lacks the content-change
// guard — the shape every database created before the v4 migration has.
const v3SQL = `
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
                  CHECK (source IN ('reflection', 'chat', 'manual', 'tool', 'mcp', 'onboarding', 'decision_log')),
    tags          TEXT DEFAULT '[]',
    pinned        INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at    TEXT NOT NULL DEFAULT (datetime('now')),
    resolved_at   TEXT
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
`

// newV3DB writes a schemaVersion-3 database with one memory row, stamped at
// user_version=3, and returns its path.
func newV3DB(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open v3 db: %v", err)
	}
	defer db.Close() //nolint:errcheck
	if _, err := db.Exec(v3SQL); err != nil {
		t.Fatalf("create v3 schema: %v", err)
	}
	seed := []string{
		`INSERT INTO projects (id, path, name) VALUES ('p1', '/tmp/v3-p1', 'p1')`,
		`INSERT INTO memories (id, project_id, category, content, source) VALUES ('m1', 'p1', 'fact', 'original content here', 'mcp')`,
		`PRAGMA user_version = 3`,
	}
	for _, s := range seed {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed v3 db: %v", err)
		}
	}
	return dbPath
}

// totalChanges reads SQLite's total_changes() counter on the given pinned
// connection: the count of rows written since the connection opened,
// including rows written by trigger bodies (unlike changes(), which counts
// only the outermost statement and excludes trigger-driven writes). This is
// the only reliable way to observe whether memories_au's body actually ran —
// a delete+re-insert of byte-identical FTS content is otherwise invisible
// from the row data alone, since the shadow tables end up in the same state
// whether or not the trigger fired.
func totalChanges(t *testing.T, ctx context.Context, conn *sql.Conn) int64 {
	t.Helper()
	var n int64
	if err := conn.QueryRowContext(ctx, `SELECT total_changes()`).Scan(&n); err != nil {
		t.Fatalf("total_changes: %v", err)
	}
	return n
}

// TestMigrateV4NarrowsUpdateTrigger: a schemaVersion-3 database's memories_au
// trigger fires unconditionally on every UPDATE, doing a wasteful FTS
// delete+re-insert even when content is untouched — e.g. SET pinned, SET
// resolved_at, SET access_count (#286). Migrating to v4 must narrow it with a
// WHEN old.content != new.content guard, while a genuine content change must
// still refresh FTS exactly as before (don't regress the trigger's purpose).
func TestMigrateV4NarrowsUpdateTrigger(t *testing.T) {
	dbPath := newV3DB(t)

	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB on v3 db: %v", err)
	}
	defer db.Close() //nolint:errcheck

	if v := schemaVersionOf(t, db); v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}

	// Pin a single connection: total_changes() is per-connection, and
	// SetMaxOpenConns(1) makes reuse overwhelmingly likely but not
	// contractual, so pin explicitly rather than rely on pool behavior.
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("pin conn: %v", err)
	}
	defer conn.Close() //nolint:errcheck

	// Primary evidence: a non-content UPDATE must cost exactly the 1 row
	// write of the UPDATE itself, with no extra trigger-driven FTS writes.
	before := totalChanges(t, ctx, conn)
	if _, err := conn.ExecContext(ctx, `UPDATE memories SET pinned = 1 WHERE id = 'm1'`); err != nil {
		t.Fatalf("update pinned: %v", err)
	}
	pinnedDelta := totalChanges(t, ctx, conn) - before
	if pinnedDelta != 1 {
		t.Errorf("pinned-only update total_changes delta = %d, want 1 (trigger fired when it should not have)", pinnedDelta)
	}

	// A genuine content change must still cost strictly more: the trigger
	// firing and rewriting the FTS shadow tables on top of the base update.
	before = totalChanges(t, ctx, conn)
	if _, err := conn.ExecContext(ctx, `UPDATE memories SET content = 'updated content here' WHERE id = 'm1'`); err != nil {
		t.Fatalf("update content: %v", err)
	}
	contentDelta := totalChanges(t, ctx, conn) - before
	if contentDelta <= pinnedDelta {
		t.Errorf("content-changing update total_changes delta = %d, want > %d (trigger should have fired)", contentDelta, pinnedDelta)
	}

	// FTS actually reflects the new content, not just "some write happened".
	var n int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM memories_fts WHERE memories_fts MATCH 'updated'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("fts match on updated content: n=%d err=%v, want 1", n, err)
	}
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM memories_fts WHERE memories_fts MATCH 'original'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("fts still matches stale content: n=%d err=%v, want 0", n, err)
	}

	// Secondary evidence: the trigger definition itself carries the guard.
	// If this fails while the delta assertions above pass, that points to a
	// marker/whitespace mismatch in this check rather than a real regression.
	// Uses the pinned conn, not db: db's pool has exactly 1 connection (see
	// OpenDB's SetMaxOpenConns(1)), which conn is still holding, so a db.*
	// call here would block forever waiting for a connection that only this
	// same, still-running function could ever release.
	var auSQL string
	if err := conn.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type='trigger' AND name='memories_au'`,
	).Scan(&auSQL); err != nil {
		t.Fatalf("read memories_au trigger SQL: %v", err)
	}
	if !strings.Contains(auSQL, "old.content != new.content") {
		t.Errorf("memories_au trigger missing content-change guard, got: %s", auSQL)
	}
}
