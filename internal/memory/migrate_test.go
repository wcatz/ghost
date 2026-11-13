package memory

import (
	"database/sql"
	"fmt"
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

// v5Schema is the v5-vintage schema the orphan tests seed: the tables migrate()
// continues through to the current version (memory_snapshots exists in a real
// database of this vintage, without the identity columns v13 adds).
var v5Schema = []string{
	`CREATE TABLE projects (
    id          TEXT PRIMARY KEY,
    path        TEXT NOT NULL UNIQUE,
    name        TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
)`,
	`CREATE TABLE memories (
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
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at    TEXT NOT NULL DEFAULT (datetime('now')),
    resolved_at   TEXT
)`,
	`CREATE TABLE memory_embeddings (
    memory_id   TEXT PRIMARY KEY REFERENCES memories(id) ON DELETE CASCADE,
    embedding   BLOB NOT NULL,
    model       TEXT NOT NULL DEFAULT 'nomic-embed-text',
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
)`,
	`CREATE TABLE link_scans (
    memory_id  TEXT PRIMARY KEY REFERENCES memories(id) ON DELETE CASCADE,
    scanned_at TEXT NOT NULL DEFAULT (datetime('now'))
)`,
	`CREATE TABLE ghost_state (
    project_id          TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    interaction_count   INTEGER NOT NULL DEFAULT 0,
    learned_context     TEXT DEFAULT '',
    last_reflection_at  TEXT,
    reflection_summary  TEXT DEFAULT '',
    updated_at          TEXT NOT NULL DEFAULT (datetime('now'))
)`,
	`CREATE TABLE memory_snapshots (
    id            TEXT PRIMARY KEY DEFAULT (hex(randomblob(16))),
    snapshot_id   TEXT NOT NULL,
    project_id    TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    category      TEXT NOT NULL,
    content       TEXT NOT NULL,
    importance    REAL NOT NULL,
    source        TEXT NOT NULL,
    tags          TEXT DEFAULT '[]',
    created_at    TEXT NOT NULL DEFAULT (datetime('now'))
)`,
}

// seedV5 opens a fresh database and applies the v5 schema plus the given rows,
// stamped at user_version 5.
func seedV5(t *testing.T, rows ...string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "ghost.db"))
	if err != nil {
		t.Fatalf("open v5 db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, s := range append(append(append([]string{}, v5Schema...), rows...), `PRAGMA user_version = 5`) {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed v5 db: %v", err)
		}
	}
	return db
}

// TestMigrateRepairsPreExistingCacheOrphans: a v5 database whose only orphans
// are derived-cache rows (link_scans/memory_embeddings for deleted memories)
// migrates cleanly: the debris is deleted, the surviving rows are untouched, and
// the DB is not bricked.
func TestMigrateRepairsPreExistingCacheOrphans(t *testing.T) {
	db := seedV5(t,
		`INSERT INTO projects (id, path, name) VALUES ('p1', '/tmp/v5c-p1', 'p1')`,
		`INSERT INTO memories (id, project_id, content) VALUES ('m1', 'p1', 'survivor memory')`,
		`INSERT INTO link_scans (memory_id, scanned_at) VALUES ('deleted-mem', datetime('now'))`,
		`INSERT INTO memory_embeddings (memory_id, embedding) VALUES ('deleted-mem', X'0102')`)
	if err := migrate(db, 5); err != nil {
		t.Fatalf("migrate v5->current with cache orphans: %v", err)
	}
	if v := schemaVersionOf(t, db); v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}
	for table, want := range map[string]int{"link_scans": 0, "memory_embeddings": 0, "memories": 1} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil || n != want {
			t.Errorf("%s after repair: n=%d err=%v, want %d", table, n, err, want)
		}
	}
}

// TestMigrateRepairsAnOrphanedEvidenceRow: the evidence table is in the
// derived-cache whitelist, and this is the case that puts it there. An evidence
// row whose memory is gone is debris by the table's own definition (the foreign
// key cascades), and unwhitelisted it would abort every later migration as user
// content. The store must be USABLE afterwards, not merely open, and the live
// row's evidence is not collateral.
func TestMigrateRepairsAnOrphanedEvidenceRow(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	// A bare DSN, deliberately: foreign_keys is off here, which is exactly how a
	// row becomes an orphan at all.
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if _, err := db.Exec(initSQL); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	for _, s := range []string{
		`INSERT INTO projects (id, path, name) VALUES ('p1', '/tmp/v18-orphan', 'p1')`,
		`INSERT INTO memories (id, project_id, content, source) VALUES ('m1', 'p1', 'a live fact', 'mcp')`,
		`INSERT INTO memory_provenance (memory_id, kind, agent, observed_at)
		 VALUES ('m1', 'observed', 'claude-code', datetime('now'))`,
		`INSERT INTO memory_provenance (memory_id, kind, agent, observed_at)
		 VALUES ('deleted-elsewhere', 'observed', 'codex', datetime('now'))`,
		`PRAGMA user_version = 17`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed (%s): %v", s, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close the seeding handle: %v", err)
	}

	// The orphan is provably there before the open, or the test proves nothing.
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	var orphans int
	if err := raw.QueryRow(`SELECT count(*) FROM memory_provenance WHERE memory_id = 'deleted-elsewhere'`).Scan(&orphans); err != nil {
		t.Fatalf("count the orphan: %v", err)
	}
	if orphans != 1 {
		t.Fatalf("the fixture has %d orphan row(s), want 1", orphans)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	opened, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB refused a store whose only orphan is derived debris: %v", err)
	}
	defer opened.Close() //nolint:errcheck
	if v := schemaVersionOf(t, opened); v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}
	if err := opened.QueryRow(`SELECT count(*) FROM memory_provenance WHERE memory_id = 'deleted-elsewhere'`).Scan(&orphans); err != nil {
		t.Fatalf("count the orphan after the repair: %v", err)
	}
	if orphans != 0 {
		t.Errorf("%d orphaned evidence row(s) survived the repair", orphans)
	}
	// The live row's evidence is NOT collateral: the repair deletes rows, not
	// evidence.
	var kept int
	if err := opened.QueryRow(`SELECT count(*) FROM memory_provenance WHERE memory_id = 'm1' AND agent = 'claude-code'`).Scan(&kept); err != nil {
		t.Fatalf("count the surviving evidence: %v", err)
	}
	if kept != 1 {
		t.Errorf("the live memory's evidence has %d row(s), want its 1", kept)
	}
	// And the store still writes, which is what "not bricked" has to mean for a
	// table the repair just deleted from.
	if _, err := opened.Exec(
		`INSERT INTO memory_provenance (memory_id, kind, agent, observed_at) VALUES ('m1', 'observed', 'goose', datetime('now'))`,
	); err != nil {
		t.Errorf("the store cannot write evidence after the repair: %v", err)
	}
}

// TestMigrateBlocksUserContentOrphans: an orphan in a user-content table (a
// memories row whose project is gone) must NOT be auto-deleted: migrate fails with
// copy-pasteable DELETE statements naming the offending rows, so the operator
// decides. Derived-cache debris self-heals; user content never silently disappears.
func TestMigrateBlocksUserContentOrphans(t *testing.T) {
	db := seedV5(t,
		`INSERT INTO projects (id, path, name) VALUES ('p1', '/tmp/v5b-p1', 'p1')`,
		`INSERT INTO memories (id, project_id, content) VALUES ('m1', 'p1', 'survivor memory')`,
		`INSERT INTO memories (id, project_id, content) VALUES ('m-orphan', 'gone-project', 'orphaned memory')`)
	err := migrate(db, 5)
	if err == nil || !strings.Contains(err.Error(), "DELETE FROM memories WHERE rowid") {
		t.Fatalf("migrate on a DB with a user-content orphan = %v, want an error with copy-pasteable DELETE statements", err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM memories`).Scan(&n); err != nil || n != 2 {
		t.Errorf("memories after blocked migrate: n=%d err=%v, want 2 (nothing auto-deleted)", n, err)
	}
	if v := schemaVersionOf(t, db); v != 5 {
		t.Errorf("user_version = %d, want 5 (migration aborted before stamping)", v)
	}
}

// TestMigrateV5DropsConversationTables (#347): a schemaVersion-4 database still
// carrying the orphaned assistant-era conversations/messages rows lands at the
// current version with both tables and their indices gone, while the surviving
// tables keep their data.
func TestMigrateV5DropsConversationTables(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	seed, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open v4 db: %v", err)
	}
	for _, stmt := range []string{
		initSQL,
		`CREATE TABLE conversations (
    id          TEXT PRIMARY KEY DEFAULT (hex(randomblob(16))),
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    mode        TEXT NOT NULL DEFAULT 'chat'
)`,
		`CREATE INDEX idx_conversations_project ON conversations(project_id)`,
		`CREATE TABLE messages (
    id              TEXT PRIMARY KEY DEFAULT (hex(randomblob(16))),
    conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    role            TEXT NOT NULL,
    content         TEXT NOT NULL
)`,
		`CREATE INDEX idx_messages_conv ON messages(conversation_id)`,
		`INSERT INTO projects (id, path, name) VALUES ('p1', '/tmp/v4-p1', 'p1')`,
		`INSERT INTO memories (id, project_id, category, content, source) VALUES ('m1', 'p1', 'fact', 'memory that must survive the drop', 'mcp')`,
		`INSERT INTO conversations (id, project_id) VALUES ('c1', 'p1')`,
		`INSERT INTO messages (conversation_id, role, content) VALUES ('c1', 'user', 'pre-strip transcript row')`,
		`PRAGMA user_version = 4`,
	} {
		if _, err := seed.Exec(stmt); err != nil {
			t.Fatalf("seed v4 db: %v", err)
		}
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB on v4 db: %v", err)
	}
	defer db.Close() //nolint:errcheck
	if v := schemaVersionOf(t, db); v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}
	for _, name := range []string{"conversations", "messages", "idx_conversations_project", "idx_messages_conv"} {
		var c int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name=?`, name).Scan(&c); err != nil || c != 0 {
			t.Errorf("%s still present after migration (c=%d err=%v)", name, c, err)
		}
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM memories WHERE id = 'm1'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("memories after migration: n=%d err=%v, want 1", n, err)
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

// TestMigrateV17AddsMemoryHistory exercises the one-shot upgrade every v16
// database takes: migrateV17 must create memory_history and both indexes, stamp
// the version and leave existing rows untouched. The fixture drops what initSQL
// created and calls migrate() directly, so the step's own DDL is what is proved.
func TestMigrateV17AddsMemoryHistory(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open v16 db: %v", err)
	}
	defer db.Close() //nolint:errcheck

	if _, err := db.Exec(initSQL); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	for _, drop := range []string{
		`DROP INDEX idx_history_memory`,
		// A recorded_at index under either name it has had is dropped too: this
		// step does not create it, and a stale one would break the absence
		// assertion below for an unrelated reason.
		`DROP INDEX IF EXISTS idx_history_recorded`,
		`DROP INDEX IF EXISTS idx_provenance_recorded`,
		`DROP TABLE memory_history`,
	} {
		if _, err := db.Exec(drop); err != nil {
			t.Fatalf("%s: %v", drop, err)
		}
	}
	seed := []string{
		`INSERT INTO projects (id, path, name) VALUES ('p1', '/tmp/v17-p1', 'p1')`,
		`INSERT INTO memories (project_id, category, content, source) VALUES ('p1', 'fact', 'a pre-v17 fact', 'mcp')`,
		`PRAGMA user_version = 16`,
	}
	for _, s := range seed {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed v16 db (%s): %v", s, err)
		}
	}

	if err := migrate(db, 16); err != nil {
		t.Fatalf("migrate v16->v17: %v", err)
	}
	if v := schemaVersionOf(t, db); v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}

	for _, obj := range []struct{ typ, name string }{
		{"table", "memory_history"},
		{"index", "idx_history_memory"},
	} {
		var n int
		if err := db.QueryRow(
			`SELECT count(*) FROM sqlite_master WHERE type=? AND name=?`, obj.typ, obj.name,
		).Scan(&n); err != nil || n != 1 {
			t.Errorf("%s %s after migrateV17: n=%d err=%v, want 1", obj.typ, obj.name, n, err)
		}
	}

	// The index list is an absence as much as a presence, and it is asserted on
	// the MIGRATION path, not only on the fresh-database one. initSQL and
	// migrateV17 are two copies of the same DDL; when they disagree the fresh
	// install and the upgraded store get different schemas, and the difference is
	// invisible to a test that only opens a new database. This one caught exactly
	// that: the index was removed from initSQL and left in migrateV17.
	var strayIndexes int
	if err := db.QueryRow(`
		SELECT count(*) FROM sqlite_master
		WHERE type='index' AND tbl_name='memory_history'
		  AND name NOT LIKE 'sqlite_autoindex%' AND name <> 'idx_history_memory'`,
	).Scan(&strayIndexes); err != nil {
		t.Fatalf("count history-table indexes: %v", err)
	}
	if strayIndexes != 0 {
		t.Errorf("migrateV17 left %d index(es) on memory_history that nothing reads; "+
			"every append pays a b-tree insert for each", strayIndexes)
	}

	// The migrated table is usable, and its phase vocabulary is the one the
	// writers send — a migration that delivered a table with a different CHECK
	// would fail on the first real save, in production, on somebody's data.
	if _, err := db.Exec(`
		INSERT INTO memory_history (memory_id, project_id, phase, content, category, importance, source)
		SELECT id, project_id, 'save', content, category, importance, source FROM memories
	`); err != nil {
		t.Fatalf("insert after migrateV17: %v", err)
	}
	var phase, content string
	if err := db.QueryRow(
		`SELECT phase, content FROM memory_history LIMIT 1`,
	).Scan(&phase, &content); err != nil {
		t.Fatalf("select from memory_history: %v", err)
	}
	if phase != "save" || content != "a pre-v17 fact" {
		t.Errorf("row = (%q, %q), want (save, a pre-v17 fact)", phase, content)
	}

	// The pre-migration memory survived the additive step untouched.
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM memories`).Scan(&n); err != nil || n != 1 {
		t.Errorf("memories after migration: n=%d err=%v, want 1", n, err)
	}
}

// TestMigrateV18SeedsEvidenceFromTheColumnsMemoriesAlreadyHeld: the one-shot
// upgrade every existing v17 database takes. migrateV18 creates
// memory_provenance and seeds it from the per-row provenance columns, because a
// store that has been recording `agent` for months would otherwise start
// reporting "no recorded evidence" about rows whose authorship it already knows.
//
// Three properties of the seed are the whole test, and each is a way a
// hand-written backfill goes wrong:
//
//   - a memory with NONE of agent/session_id/source_ref/confidence gets NO row.
//     A row that says nothing asserts an observation, and there was none.
//   - a memory with some of them gets a row holding exactly those, with the rest
//     NULL. The seed copies; it never fills a gap.
//   - verified_at comes across when the memory has one, because a memory someone
//     verified by hand IS corroborated, and dropping the stamp would understate
//     the support the store already records.
func TestMigrateV18SeedsEvidenceFromTheColumnsMemoriesAlreadyHeld(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open v17 db: %v", err)
	}
	defer db.Close() //nolint:errcheck

	if _, err := db.Exec(initSQL); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	for _, drop := range []string{
		`DROP INDEX idx_provenance_memory`,
		`DROP TABLE memory_provenance`,
	} {
		if _, err := db.Exec(drop); err != nil {
			t.Fatalf("%s: %v", drop, err)
		}
	}
	seed := []string{
		`INSERT INTO projects (id, path, name) VALUES ('p1', '/tmp/v18-p1', 'p1')`,
		// Full provenance, plus a hand verification.
		`INSERT INTO memories (id, project_id, category, content, source, agent, session_id, source_ref, confidence, verified_at)
		 VALUES ('m-full', 'p1', 'fact', 'a fully attributed fact', 'mcp', 'claude-code', 'ses_a', 'docs/x.md:L3', 0.8, '2026-01-02 03:04:05')`,
		// One field only: the row is worth making, and the other three stay NULL.
		`INSERT INTO memories (id, project_id, category, content, source, agent)
		 VALUES ('m-agent', 'p1', 'fact', 'a fact with only an agent', 'mcp', 'codex')`,
		// A confidence with no agent: still evidence, still not a session.
		`INSERT INTO memories (id, project_id, category, content, source, confidence)
		 VALUES ('m-conf', 'p1', 'fact', 'a fact with only a confidence', 'mcp', 0.4)`,
		// Verified, but nothing else: no provenance claim to carry, and the stamp
		// is already on the memory itself.
		`INSERT INTO memories (id, project_id, category, content, source, verified_at)
		 VALUES ('m-verified', 'p1', 'fact', 'a verified fact with no provenance', 'manual', '2026-02-03 04:05:06')`,
		// Nothing recorded at all: no row.
		`INSERT INTO memories (id, project_id, category, content, source)
		 VALUES ('m-bare', 'p1', 'fact', 'a fact nobody attributed', 'mcp')`,
		`PRAGMA user_version = 17`,
	}
	for _, s := range seed {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed v17 db (%s): %v", s, err)
		}
	}

	if err := migrate(db, 17); err != nil {
		t.Fatalf("migrate v17->v18: %v", err)
	}
	if v := schemaVersionOf(t, db); v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}

	for _, obj := range []struct{ typ, name string }{
		{"table", "memory_provenance"},
		{"index", "idx_provenance_memory"},
	} {
		var n int
		if err := db.QueryRow(
			`SELECT count(*) FROM sqlite_master WHERE type=? AND name=?`, obj.typ, obj.name,
		).Scan(&n); err != nil || n != 1 {
			t.Errorf("%s %s after migrateV18: n=%d err=%v, want 1", obj.typ, obj.name, n, err)
		}
	}

	// One index and no more, asserted on the migration path: initSQL and
	// migrateV18 are two copies of one DDL, and when they disagree the fresh
	// install and the upgraded store get different schemas. Every append would
	// pay a b-tree insert per extra index, inside the write transaction.
	var stray int
	if err := db.QueryRow(`
		SELECT count(*) FROM sqlite_master
		WHERE type='index' AND tbl_name='memory_provenance'
		  AND name NOT LIKE 'sqlite_autoindex%' AND name <> 'idx_provenance_memory'`,
	).Scan(&stray); err != nil {
		t.Fatalf("count evidence indexes: %v", err)
	}
	if stray != 0 {
		t.Errorf("migrateV18 left %d index(es) on memory_provenance that nothing reads", stray)
	}

	// The full-provenance row, read back field by field through IS NULL so an
	// empty string cannot pass for an honest absence.
	var (
		kind, agent, sessionID, sourceRef string
		confidence                        float64
		verifiedAt                        sql.NullString
		observedAt                        sql.NullString
	)
	if err := db.QueryRow(`
		SELECT kind, agent, session_id, source_ref, confidence, verified_at, observed_at
		FROM memory_provenance WHERE memory_id = 'm-full'`,
	).Scan(&kind, &agent, &sessionID, &sourceRef, &confidence, &verifiedAt, &observedAt); err != nil {
		t.Fatalf("read the seeded row: %v", err)
	}
	if kind != "legacy" {
		t.Errorf("kind = %q, want legacy — the seed is not an observation this build made", kind)
	}
	if agent != "claude-code" || sessionID != "ses_a" || sourceRef != "docs/x.md:L3" || confidence != 0.8 {
		t.Errorf("seeded row = (%q, %q, %q, %v), want the memory's own columns", agent, sessionID, sourceRef, confidence)
	}
	if !verifiedAt.Valid || verifiedAt.String != "2026-01-02 03:04:05" {
		t.Errorf("verified_at = %v, want the memory's own stamp", verifiedAt)
	}
	// The seed states no moment: it read the columns at migration time, and
	// stamping that as when the fact was observed would be a claim about the
	// past nobody made.
	if observedAt.Valid {
		t.Errorf("observed_at = %q, want NULL — the migration cannot know when the fact was observed", observedAt.String)
	}

	// The one-field row exists, and the other three are absent rather than empty.
	var nulls int
	if err := db.QueryRow(`
		SELECT (session_id IS NULL) + (source_ref IS NULL) + (confidence IS NULL)
		FROM memory_provenance WHERE memory_id = 'm-agent'`,
	).Scan(&nulls); err != nil {
		t.Fatalf("read the agent-only row: %v", err)
	}
	if nulls != 3 {
		t.Errorf("%d of session_id/source_ref/confidence are NULL on the agent-only row, want 3", nulls)
	}
	if err := db.QueryRow(
		`SELECT agent FROM memory_provenance WHERE memory_id = 'm-agent'`,
	).Scan(&agent); err != nil {
		t.Fatalf("read the agent-only row's agent: %v", err)
	}
	if agent != "codex" {
		t.Errorf("agent = %q, want codex", agent)
	}

	// A confidence with no agent is still a claim somebody made, and the agent
	// stays unknown.
	if err := db.QueryRow(`
		SELECT (agent IS NULL) + (session_id IS NULL) + (source_ref IS NULL), confidence
		FROM memory_provenance WHERE memory_id = 'm-conf'`,
	).Scan(&nulls, &confidence); err != nil {
		t.Fatalf("read the confidence-only row: %v", err)
	}
	if nulls != 3 || confidence != 0.4 {
		t.Errorf("confidence-only row = (%d nulls, %v), want 3 nulls and 0.4", nulls, confidence)
	}

	// A memory that recorded no provenance gets no evidence row, and the memory
	// carrying only a verification stamp does not either: the stamp is on the
	// memory, and the row exists to carry a provenance claim.
	var seeded int
	if err := db.QueryRow(`SELECT count(*) FROM memory_provenance`).Scan(&seeded); err != nil {
		t.Fatalf("count seeded rows: %v", err)
	}
	if seeded != 3 {
		t.Errorf("seeded %d evidence rows, want 3 — a row for a memory that recorded nothing is an observation nobody made", seeded)
	}
	for _, id := range []string{"m-bare", "m-verified"} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM memory_provenance WHERE memory_id = ?`, id).Scan(&n); err != nil {
			t.Fatalf("count rows for %s: %v", id, err)
		}
		if n != 0 {
			t.Errorf("%s got %d evidence row(s), want none", id, n)
		}
	}

	// The memories themselves are untouched: the step is additive.
	var memories int
	if err := db.QueryRow(`SELECT count(*) FROM memories`).Scan(&memories); err != nil || memories != 5 {
		t.Errorf("memories after migration: n=%d err=%v, want 5", memories, err)
	}

	// The seeded table takes what the writers send, and refuses what no writer
	// can mean. A migration that delivered a table with a different CHECK would
	// fail on the first real save, in production, on somebody's data.
	if _, err := db.Exec(`
		INSERT INTO memory_provenance (memory_id, kind, agent, observed_at)
		VALUES ('m-full', 'observed', 'claude-code', datetime('now'))`); err != nil {
		t.Fatalf("insert after migrateV18: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO memory_provenance (memory_id, kind) VALUES ('m-full', 'guessed')`); err == nil {
		t.Error("the kind CHECK accepted a value no writer emits; a phase nothing reads is a phase no filter can use")
	}
	// The cascade is not asserted here: this fixture's handle was opened with a
	// bare DSN, so foreign_keys is off and the pragma cannot show anything about
	// it. TestDeleteCascadesEvidenceAndKeepsTheHistoryTombstone proves the
	// cascade on a handle that has it on.
}

// TestMigrateV18RefusesADevTableUnderTheEvidenceName: the name was reserved
// before it was used, and a build between two commits used it for the CHANGE LOG
// instead (#664's pre-rename shape). initSQL's CREATE INDEX is a no-op against
// such a table — the change log has a memory_id too — so nothing before the seed
// stops the INSERT, and without the step's own check the operator gets "no such
// column: kind", a rolled-back step and a store that will not open.
//
// The check REFUSES, and it has to name the way out: the rows in that table are a
// dev build's change log under the wrong name, there is no shape to convert them
// into, and adapting them would be guessing. A warning nobody may see leaves every
// writer failing on the same missing column.
func TestMigrateV18RefusesADevTableUnderTheEvidenceName(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close() //nolint:errcheck

	if _, err := db.Exec(initSQL); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	for _, drop := range []string{`DROP INDEX idx_provenance_memory`, `DROP TABLE memory_provenance`} {
		if _, err := db.Exec(drop); err != nil {
			t.Fatalf("%s: %v", drop, err)
		}
	}
	// The pre-rename #664 shape: a change log under the reserved name. It has a
	// memory_id (so initSQL's index creates cleanly against it) and no kind.
	if _, err := db.Exec(`
		CREATE TABLE memory_provenance (
			id          TEXT PRIMARY KEY,
			memory_id   TEXT NOT NULL,
			project_id  TEXT NOT NULL,
			recorded_at TEXT NOT NULL,
			phase       TEXT NOT NULL,
			content     TEXT
		)`); err != nil {
		t.Fatalf("create the dev table: %v", err)
	}
	for _, s := range []string{
		`INSERT INTO projects (id, path, name) VALUES ('p1', '/tmp/v18-dev', 'p1')`,
		`INSERT INTO memories (project_id, category, content, source) VALUES ('p1', 'fact', 'a pre-v18 fact', 'mcp')`,
		`PRAGMA user_version = 17`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed (%s): %v", s, err)
		}
	}

	err = migrate(db, 17)
	if err == nil {
		t.Fatal("migrate accepted a memory_provenance table that is not the evidence table")
	}
	msg := err.Error()
	for _, want := range []string{"kind", "DROP TABLE memory_provenance", "development build"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error = %q, want it to name %q", msg, want)
		}
	}
	// The refusal changes nothing: the dev table and corpus are untouched and the
	// version is not stamped.
	if v := schemaVersionOf(t, db); v != 17 {
		t.Errorf("user_version = %d after a refused migration, want 17", v)
	}
	var rows int
	if err := db.QueryRow(`SELECT count(*) FROM memory_provenance`).Scan(&rows); err != nil {
		t.Fatalf("read the dev table: %v", err)
	}
	if rows != 0 {
		t.Errorf("the refused migration wrote %d row(s) into the dev table", rows)
	}
	// The OPEN path is where the cost lives: a check only inside migrateV18 would
	// write a VACUUM INTO copy on every open of a store that cannot be opened, and
	// a second open in the same second would fail on the copy's name without
	// naming the table or the remedy. So: no copy, and the retry says the same.
	for attempt := range 2 {
		if _, err := OpenDB(dbPath); err == nil {
			t.Fatalf("attempt %d: OpenDB opened a store holding a foreign memory_provenance table", attempt)
		} else {
			msg := err.Error()
			for _, want := range []string{"memory_provenance", "DROP TABLE memory_provenance"} {
				if !strings.Contains(msg, want) {
					t.Errorf("attempt %d: error = %q, want it to name %q", attempt, msg, want)
				}
			}
			if strings.Contains(msg, "pre-migrate") {
				t.Errorf("attempt %d: the refusal came from the backup path, so it cost a copy: %v", attempt, err)
			}
		}
	}
	backups, err := filepath.Glob(dbPath + ".pre-migrate-*")
	if err != nil {
		t.Fatalf("glob backups: %v", err)
	}
	if len(backups) != 0 {
		t.Errorf("a refused open wrote %d pre-migration backup(s): %v", len(backups), backups)
	}
	if v := schemaVersionOf(t, db); v != 17 {
		t.Errorf("user_version = %d after two refused opens, want 17", v)
	}

	// And the documented way out works: drop it and the step runs.
	if _, err := db.Exec(`DROP TABLE memory_provenance`); err != nil {
		t.Fatalf("drop the dev table: %v", err)
	}
	if err := migrate(db, 17); err != nil {
		t.Fatalf("migrate after dropping the dev table: %v", err)
	}
	if v := schemaVersionOf(t, db); v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}
	var seeded int
	if err := db.QueryRow(`SELECT count(*) FROM memory_provenance WHERE kind = 'legacy'`).Scan(&seeded); err != nil {
		t.Fatalf("count seeded rows: %v", err)
	}
	if seeded != 0 {
		t.Errorf("%d legacy row(s) seeded, want none — the fixture's memory recorded no provenance", seeded)
	}
}

// TestTheEvidenceShapeProbeAsksWhetherItIsTheEvidenceTable: the refusal above
// must fire on a table that is NOT the evidence table and must NOT fire on one that
// is an older version of it.
//
// That distinction is the whole design of the probe, and getting it wrong fails in
// the worst direction available. Its job is to catch a pre-rename #664 dev build
// that used the reserved name for the CHANGE LOG — and `kind` alone settles that,
// because no shape of the change log has one. Listing every column instead would
// make the probe a VERSION check wearing an identity check's clothes: the next
// schema change that adds a column to this table would then refuse every store
// already at that version, and the remedy it names is `DROP TABLE
// memory_provenance` — a command that destroys real evidence records. A future
// build must not answer "your table is the wrong table" about a table it merely
// knows an older shape of.
func TestTheEvidenceShapeProbeAsksWhetherItIsTheEvidenceTable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if _, err := db.Exec(initSQL); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	for _, drop := range []string{`DROP INDEX idx_provenance_memory`, `DROP TABLE memory_provenance`} {
		if _, err := db.Exec(drop); err != nil {
			t.Fatalf("%s: %v", drop, err)
		}
	}
	// The evidence table as an EARLIER build wrote it (no carried_from): its rows
	// are real evidence, and a probe reading the column list as a version check
	// would tell an operator to drop them.
	if _, err := db.Exec(`
		CREATE TABLE memory_provenance (
    id          TEXT PRIMARY KEY DEFAULT (hex(randomblob(16))),
    memory_id   TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
    kind        TEXT NOT NULL CHECK (kind IN ('observed','imported','verified','legacy')),
    agent       TEXT,
    session_id  TEXT,
    source_ref  TEXT,
    confidence  REAL,
    observed_at TEXT,
    verified_at TEXT
	)`); err != nil {
		t.Fatalf("create the older evidence table: %v", err)
	}
	for _, s := range []string{
		`INSERT INTO projects (id, path, name) VALUES ('p1', '/tmp/v18-old', 'p1')`,
		`INSERT INTO memories (id, project_id, category, content, source, agent)
		 VALUES ('m1', 'p1', 'fact', 'an older fact', 'mcp', 'claude-code')`,
		`INSERT INTO memory_provenance (memory_id, kind, agent, observed_at)
		 VALUES ('m1', 'observed', 'claude-code', datetime('now'))`,
		`PRAGMA user_version = 17`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed (%s): %v", s, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	opened, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB refused a store whose evidence table is an older shape of ours: %v", err)
	}
	defer opened.Close() //nolint:errcheck
	var kept int
	if err := opened.QueryRow(`SELECT count(*) FROM memory_provenance WHERE agent = 'claude-code'`).Scan(&kept); err != nil {
		t.Fatalf("count the evidence: %v", err)
	}
	if kept != 1 {
		t.Errorf("%d evidence row(s) survived, want 1", kept)
	}
	if _, err := opened.Exec(`SELECT carried_from FROM memory_provenance LIMIT 0`); err == nil {
		t.Error("the older table somehow has carried_from; the fixture no longer tests what it claims")
	}

	// A table with no `kind` is NOT the evidence table and is still refused.
	if err := refuseForeignProvenanceTable(opened); err != nil {
		t.Errorf("the probe refused the evidence table itself: %v", err)
	}
	notEvidence, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer notEvidence.Close() //nolint:errcheck
	if _, err := notEvidence.Exec(`ALTER TABLE memory_provenance RENAME TO memory_provenance_saved`); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, err := notEvidence.Exec(`
		CREATE TABLE memory_provenance (
    id        TEXT PRIMARY KEY,
    memory_id TEXT NOT NULL,
    phase     TEXT NOT NULL,
    content   TEXT
	)`); err != nil {
		t.Fatalf("create the change-log-shaped table: %v", err)
	}
	if err := refuseForeignProvenanceTable(notEvidence); err == nil {
		t.Error("the probe accepted a change log under the evidence name; it has no kind column")
	} else if !strings.Contains(err.Error(), "kind") {
		t.Errorf("error = %q, want it to name the column that is missing", err)
	}
}

// TestMigrateV20MatchesTheFreshDatabaseSchema: the migration path and the initSQL
// path must produce the same table, or a store that reached v20 by upgrading
// holds a different shape from one created fresh at the same version.
//
// Both spellings of the DDL exist — initSQL cannot deliver a new table to a
// database that already exists — so nothing but this comparison keeps them in
// step. It reads the column set WITH its types and NOT NULL flags, because a
// column that exists with the wrong constraint is a different schema, and a
// column added to one path and not the other reaches production as "works on
// new installs, empty on upgraded ones".
//
// The migration step is RUN DIRECTLY rather than reached through OpenDB, and the
// reason is worth stating because getting it wrong makes the test vacuous:
// OpenDB executes initSQL BEFORE the migrations, so a database that has had the
// table dropped already has it back again by the time migrateV20 runs and the
// step is a no-op. Both arms are built here from initSQL alone, and the migrated
// arm has the table removed and migrateV20's own statements applied to it.
func TestMigrateV20MatchesTheFreshDatabaseSchema(t *testing.T) {
	newDB := func(t *testing.T) *sql.DB {
		t.Helper()
		db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "ghost.db"))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if _, err := db.Exec(initSQL); err != nil {
			t.Fatalf("initSQL: %v", err)
		}
		return db
	}

	fresh := newDB(t)
	migrated := newDB(t)
	if _, err := migrated.Exec(`DROP TABLE IF EXISTS retrieval_record`); err != nil {
		t.Fatalf("drop the table so the step has work to do: %v", err)
	}
	tx, err := migrated.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := migrateV20(tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migrateV20: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	freshCols, err := columnShapes(t, fresh, "retrieval_record")
	if err != nil {
		t.Fatalf("read the fresh table: %v", err)
	}
	if len(freshCols) == 0 {
		t.Fatal("a fresh database has no retrieval_record — the initSQL path is missing the table")
	}
	migratedCols, err := columnShapes(t, migrated, "retrieval_record")
	if err != nil {
		t.Fatalf("read the migrated table: %v", err)
	}
	if diff := columnShapeDiff(freshCols, migratedCols); diff != "" {
		t.Errorf("the migration path and initSQL disagree on the table: %s", diff)
	}
	// And the index, which the same repetition carries: a migrated store without
	// it reads the whole table for the one query that filters by project.
	var idx string
	if err := migrated.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_retrieval_record_project'`,
	).Scan(&idx); err != nil {
		t.Errorf("migrateV20 did not create the project index: %v", err)
	}
}

// columnShapes is one table's columns as "name type notnull default" lines, in
// the order PRAGMA reports them. Types and constraints are included because a
// column that exists with the wrong NOT NULL is a different schema.
func columnShapes(t *testing.T, db *sql.DB, table string) ([]string, error) {
	t.Helper()
	rows, err := db.Query(`SELECT name, type, "notnull", dflt_value FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []string
	for rows.Next() {
		var name, typ, notnull string
		var dflt sql.NullString
		if err := rows.Scan(&name, &typ, &notnull, &dflt); err != nil {
			return nil, err
		}
		out = append(out, fmt.Sprintf("%s %s notnull=%s default=%s", name, typ, notnull, dflt.String))
	}
	return out, rows.Err()
}

// columnShapeDiff names the first difference between two column lists, or "".
func columnShapeDiff(a, b []string) string {
	if len(a) != len(b) {
		return fmt.Sprintf("%d column(s) against %d: %v vs %v", len(a), len(b), a, b)
	}
	for i := range a {
		if a[i] != b[i] {
			return fmt.Sprintf("column %d is %q on the fresh path and %q on the migrated path", i, a[i], b[i])
		}
	}
	return ""
}
