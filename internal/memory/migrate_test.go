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

// TestMigrateFreshDBHasResolveKeptHash: a brand-new database (initSQL path, no
// legacy schema involved) must have resolve_kept_hash from the start — guards
// against the column silently going missing from initSQL while migrateV7 still
// exists to paper over it on upgraded databases — and it must default to the
// empty (never-judged) hash.
func TestMigrateFreshDBHasResolveKeptHash(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close() //nolint:errcheck

	if _, err := db.Exec(`SELECT resolve_kept_hash FROM memories LIMIT 0`); err != nil {
		t.Fatalf("resolve_kept_hash column missing on fresh db: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO projects (id, path, name) VALUES ('p1', '/tmp/fresh-p1', 'p1')`,
	); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO memories (id, project_id, content) VALUES ('m1', 'p1', 'fresh note')`,
	); err != nil {
		t.Fatalf("seed memory: %v", err)
	}
	var hash string
	if err := db.QueryRow(`SELECT resolve_kept_hash FROM memories WHERE id = 'm1'`).Scan(&hash); err != nil {
		t.Fatalf("read default: %v", err)
	}
	if hash != "" {
		t.Errorf("default resolve_kept_hash = %q, want empty", hash)
	}
}

// TestMigrateFreshDBHasSupersedeChecked: a brand-new database (initSQL path, no
// legacy schema involved) must have supersede_checked from the start — guards
// against the table silently going missing from initSQL while migrateV8 still
// exists to paper over it on upgraded databases.
func TestMigrateFreshDBHasSupersedeChecked(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close() //nolint:errcheck

	if _, err := db.Exec(`SELECT newer_id, older_id, project_id, newer_hash, older_hash FROM supersede_checked LIMIT 0`); err != nil {
		t.Fatalf("supersede_checked table missing on fresh db: %v", err)
	}
	var idx string
	if err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_supersede_checked_project'`,
	).Scan(&idx); err != nil {
		t.Fatalf("idx_supersede_checked_project missing on fresh db: %v", err)
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

// TestMigrateRepairsPreExistingCacheOrphans: a v5 database whose only orphans
// are derived-cache rows — link_scans/memory_embeddings for deleted memories —
// must migrate cleanly to schemaVersion: the debris is deleted with a warning,
// the surviving rows are untouched, and the DB opens (it is not bricked).
func TestMigrateRepairsPreExistingCacheOrphans(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open v5 db: %v", err)
	}
	defer db.Close() //nolint:errcheck

	v5 := []string{
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
		// migrate() now continues to v13, which ALTERs memory_snapshots; a real
		// database of this vintage always has the table (it predates v6 — see the
		// pre-v6 schema in this file), just not the identity columns v13 adds.
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
		`INSERT INTO projects (id, path, name) VALUES ('p1', '/tmp/v5c-p1', 'p1')`,
		`INSERT INTO memories (id, project_id, content) VALUES ('m1', 'p1', 'survivor memory')`,
		`INSERT INTO link_scans (memory_id, scanned_at) VALUES ('deleted-mem', datetime('now'))`,
		`INSERT INTO memory_embeddings (memory_id, embedding) VALUES ('deleted-mem', X'0102')`,
		`PRAGMA user_version = 5`,
	}
	for _, s := range v5 {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed v5 db: %v", err)
		}
	}

	if err := migrate(db, 5); err != nil {
		t.Fatalf("migrate v5->current with cache orphans: %v", err)
	}
	if v := schemaVersionOf(t, db); v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}

	// The orphaned derived rows were deleted (repair path).
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM link_scans`).Scan(&n); err != nil || n != 0 {
		t.Errorf("link_scans after repair: n=%d err=%v, want 0", n, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM memory_embeddings`).Scan(&n); err != nil || n != 0 {
		t.Errorf("memory_embeddings after repair: n=%d err=%v, want 0", n, err)
	}
	// The surviving memory row is untouched.
	if err := db.QueryRow(`SELECT count(*) FROM memories WHERE id = 'm1'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("memories after repair: n=%d err=%v, want 1", n, err)
	}
}

// TestMigrateRepairsAnOrphanedEvidenceRow: the evidence table is in the
// derived-cache whitelist, and this is the case that puts it there.
//
// An evidence row whose memory is gone is debris by the table's OWN definition —
// the foreign key cascades, so a live store cannot hold one, and this table says
// in its schema comment that evidence without its memory means nothing. Left
// unwhitelisted it would be treated as user content: every later migration would
// ABORT on it, with copy-pasteable DELETEs, and a build one version ahead of the
// delete would refuse to open the store at all. That is the failure the whitelist
// exists to prevent, and the evidence rows it gives up are rows that a correct
// delete would already have removed.
//
// The store must be USABLE afterwards, not merely open: the repair runs before the
// migration steps, so a table left broken by it would fail the first save.
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
// memories row whose project is gone) must NOT be auto-deleted — migrate fails
// with copy-pasteable DELETE statements naming the offending rows, so the
// operator decides. This is the deliberate non-destructive side of the repair:
// derived-cache debris self-heals, user content never silently disappears.
func TestMigrateBlocksUserContentOrphans(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open v5 db: %v", err)
	}
	defer db.Close() //nolint:errcheck

	v5 := []string{
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
		`CREATE TABLE ghost_state (
    project_id          TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    interaction_count   INTEGER NOT NULL DEFAULT 0,
    learned_context     TEXT DEFAULT '',
    last_reflection_at  TEXT,
    reflection_summary  TEXT DEFAULT '',
    updated_at          TEXT NOT NULL DEFAULT (datetime('now'))
)`,
		`INSERT INTO projects (id, path, name) VALUES ('p1', '/tmp/v5b-p1', 'p1')`,
		`INSERT INTO memories (id, project_id, content) VALUES ('m1', 'p1', 'survivor memory')`,
		`INSERT INTO memories (id, project_id, content) VALUES ('m-orphan', 'gone-project', 'orphaned memory')`,
		`PRAGMA user_version = 5`,
	}
	for _, s := range v5 {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed v5 db: %v", err)
		}
	}

	err = migrate(db, 5)
	if err == nil {
		t.Fatal("migrate succeeded on DB with a user-content orphan, want error")
	}
	if !strings.Contains(err.Error(), "DELETE FROM memories WHERE rowid") {
		t.Errorf("migrate error lacks copy-pasteable DELETE statements: %v", err)
	}
	// Nothing was deleted: the survivor row is intact and so is the orphan
	// (the operator deletes it deliberately after reading the message).
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM memories`).Scan(&n); err != nil || n != 2 {
		t.Errorf("memories after blocked migrate: n=%d err=%v, want 2 (nothing auto-deleted)", n, err)
	}
	if v := schemaVersionOf(t, db); v != 5 {
		t.Errorf("user_version = %d, want 5 (migration aborted before stamping)", v)
	}
}

// TestMigrateIdempotent: opening an already-migrated database repeatedly is a no-op.
// v4WithConversationsDB writes a schemaVersion-4 database: the current
// initSQL shape plus the pre-v5 conversations/messages tables with orphaned
// rows still in them — the state of every DB created before #347's strip.
// initSQL no longer creates those two tables, so they're added explicitly to
// reproduce what a real v4 database carries; stamped at user_version=4.
func v4WithConversationsDB(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open v4 db: %v", err)
	}
	defer db.Close() //nolint:errcheck
	if _, err := db.Exec(initSQL); err != nil {
		t.Fatalf("create current schema: %v", err)
	}
	legacy := []string{
		`CREATE TABLE conversations (
    id          TEXT PRIMARY KEY DEFAULT (hex(randomblob(16))),
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    mode        TEXT NOT NULL DEFAULT 'chat',
    title       TEXT DEFAULT '',
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
)`,
		`CREATE TABLE messages (
    id              TEXT PRIMARY KEY DEFAULT (hex(randomblob(16))),
    conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    role            TEXT NOT NULL CHECK (role IN ('user', 'assistant', 'tool_use', 'tool_result')),
    content         TEXT NOT NULL,
    tool_name       TEXT,
    tool_use_id     TEXT,
    created_at      TEXT NOT NULL DEFAULT (datetime('now'))
)`,
		`INSERT INTO projects (id, path, name) VALUES ('p1', '/tmp/v4-p1', 'p1')`,
		`INSERT INTO memories (id, project_id, category, content, source) VALUES
			('m1', 'p1', 'fact', 'memory that must survive the drop', 'mcp')`,
		`INSERT INTO conversations (id, project_id, mode) VALUES ('c1', 'p1', 'chat')`,
		`INSERT INTO messages (conversation_id, role, content) VALUES
			('c1', 'user', 'pre-strip transcript row'),
			('c1', 'assistant', 'another orphaned row')`,
		`PRAGMA user_version = 4`,
	}
	for _, s := range legacy {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed v4 db: %v", err)
		}
	}
	return dbPath
}

// TestMigrateV5DropsConversationTables: a schemaVersion-4 database carrying
// the orphaned assistant-era conversations/messages rows must land at the
// current version with both tables (and their indices) gone, while every
// surviving table keeps its data untouched (#347).
func TestMigrateV5DropsConversationTables(t *testing.T) {
	dbPath := v4WithConversationsDB(t)

	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB on v4 db: %v", err)
	}
	defer db.Close() //nolint:errcheck

	if v := schemaVersionOf(t, db); v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}

	// Tables and their indices are gone entirely — not just emptied.
	for _, name := range []string{
		"conversations", "messages",
		"idx_conversations_project", "idx_messages_conv",
	} {
		var c int
		typ := "table"
		if strings.HasPrefix(name, "idx_") {
			typ = "index"
		}
		if err := db.QueryRow(
			`SELECT count(*) FROM sqlite_master WHERE type=? AND name=?`, typ, name,
		).Scan(&c); err != nil || c != 0 {
			t.Errorf("%s %s still present after migration (c=%d err=%v)", typ, name, c, err)
		}
	}

	// Surviving data is intact: the seeded project and memory rows are
	// unaffected by dropping their conversational siblings.
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM memories WHERE id = 'm1'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("memories after migration: n=%d err=%v, want 1", n, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM projects WHERE id = 'p1'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("projects after migration: n=%d err=%v, want 1", n, err)
	}

	// A fresh write into a surviving table works normally post-migration.
	if _, err := db.Exec(
		`INSERT INTO memories (id, project_id, content, source) VALUES ('m2', 'p1', 'post-migration insert', 'mcp')`,
	); err != nil {
		t.Errorf("insert after migration: %v", err)
	}
}

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

// TestMigrateV6AddsReflectInputSig exercises the one-shot upgrade every
// existing v5 database takes — through v6 and on into the later steps (the
// seed carries the tables those steps touch): migrateV6 must ALTER the old
// ghost_state shape, not rely on initSQL's fresh-database column.
func TestMigrateV6AddsReflectInputSig(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open v5 db: %v", err)
	}
	defer db.Close() //nolint:errcheck

	v5 := []string{
		`CREATE TABLE projects (
    id          TEXT PRIMARY KEY,
    path        TEXT NOT NULL UNIQUE,
    name        TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
)`,
		`CREATE TABLE ghost_state (
    project_id          TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    interaction_count   INTEGER NOT NULL DEFAULT 0,
    learned_context     TEXT DEFAULT '',
    last_reflection_at  TEXT,
    reflection_summary  TEXT DEFAULT '',
    updated_at          TEXT NOT NULL DEFAULT (datetime('now'))
)`,
		// migrate() now continues past v6 to v7, which ALTERs memories; a real
		// v5 database always has this table (with resolved_at from v2).
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
		// migrate() now continues to v13, which ALTERs memory_snapshots; a real
		// database of this vintage always has the table (it predates v6 — see the
		// pre-v6 schema in this file), just not the identity columns v13 adds.
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
		`INSERT INTO projects (id, path, name) VALUES ('p1', '/tmp/v5-p1', 'p1')`,
		`INSERT INTO ghost_state (project_id, learned_context) VALUES ('p1', 'pre-migration context')`,
		`PRAGMA user_version = 5`,
	}
	for _, s := range v5 {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed v5 db: %v", err)
		}
	}

	if err := migrate(db, 5); err != nil {
		t.Fatalf("migrate v5->current: %v", err)
	}
	if v := schemaVersionOf(t, db); v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	exists, err := columnExists(tx, "ghost_state", "reflect_input_sig")
	_ = tx.Rollback() //nolint:errcheck
	if err != nil {
		t.Fatalf("columnExists: %v", err)
	}
	if !exists {
		t.Fatal("reflect_input_sig column missing after migrateV6")
	}

	// The migrated row defaults to the empty fingerprint and its other columns
	// survive.
	var sig, learned string
	if err := db.QueryRow(
		`SELECT reflect_input_sig, learned_context FROM ghost_state WHERE project_id = 'p1'`,
	).Scan(&sig, &learned); err != nil {
		t.Fatalf("read migrated row: %v", err)
	}
	if sig != "" {
		t.Errorf("reflect_input_sig = %q, want empty", sig)
	}
	if learned != "pre-migration context" {
		t.Errorf("learned_context = %q, want preserved value", learned)
	}
}

// TestMigrateV7AddsResolveKeptHash exercises the one-shot upgrade every
// existing v6 database takes: migrateV7 must ALTER the old memories shape, not
// rely on initSQL's fresh-database column, and migrated rows must default to
// the empty (never-judged) hash.
func TestMigrateV7AddsResolveKeptHash(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open v6 db: %v", err)
	}
	defer db.Close() //nolint:errcheck

	v6 := []string{
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
		// migrate() now continues to v13, which ALTERs memory_snapshots; a real
		// database of this vintage always has the table (it predates v6 — see the
		// pre-v6 schema in this file), just not the identity columns v13 adds.
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
		`INSERT INTO projects (id, path, name) VALUES ('p1', '/tmp/v6-p1', 'p1')`,
		`INSERT INTO memories (id, project_id, content) VALUES ('m1', 'p1', 'pre-migration note')`,
		`PRAGMA user_version = 6`,
	}
	for _, s := range v6 {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed v6 db: %v", err)
		}
	}

	if err := migrate(db, 6); err != nil {
		t.Fatalf("migrate v6->v7: %v", err)
	}
	if v := schemaVersionOf(t, db); v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	exists, err := columnExists(tx, "memories", "resolve_kept_hash")
	_ = tx.Rollback() //nolint:errcheck
	if err != nil {
		t.Fatalf("columnExists: %v", err)
	}
	if !exists {
		t.Fatal("resolve_kept_hash column missing after migrateV7")
	}

	// The migrated row defaults to the empty hash and its other columns
	// survive.
	var hash, content string
	if err := db.QueryRow(
		`SELECT resolve_kept_hash, content FROM memories WHERE id = 'm1'`,
	).Scan(&hash, &content); err != nil {
		t.Fatalf("read migrated row: %v", err)
	}
	if hash != "" {
		t.Errorf("resolve_kept_hash = %q, want empty", hash)
	}
	if content != "pre-migration note" {
		t.Errorf("content = %q, want preserved value", content)
	}
}

// TestMigrateV8AddsSupersedeChecked exercises the one-shot upgrade every
// existing v7 database takes: migrateV8 must create supersede_checked (and its
// project index), not rely on initSQL's fresh-database DDL.
func TestMigrateV8AddsSupersedeChecked(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open v7 db: %v", err)
	}
	defer db.Close() //nolint:errcheck

	v7 := []string{
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
    resolved_at   TEXT,
    resolve_kept_hash TEXT NOT NULL DEFAULT ''
)`,
		// migrate() now continues to v13, which ALTERs memory_snapshots; a real
		// database of this vintage always has the table (it predates v6 — see the
		// pre-v6 schema in this file), just not the identity columns v13 adds.
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
		`INSERT INTO projects (id, path, name) VALUES ('p1', '/tmp/v7-p1', 'p1')`,
		`INSERT INTO memories (id, project_id, content) VALUES ('m1', 'p1', 'newer note')`,
		`INSERT INTO memories (id, project_id, content) VALUES ('m2', 'p1', 'older note')`,
		`PRAGMA user_version = 7`,
	}
	for _, s := range v7 {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed v7 db: %v", err)
		}
	}

	if err := migrate(db, 7); err != nil {
		t.Fatalf("migrate v7->v8: %v", err)
	}
	if v := schemaVersionOf(t, db); v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}

	var name string
	if err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='supersede_checked'`,
	).Scan(&name); err != nil {
		t.Fatalf("supersede_checked table missing after migrateV8: %v", err)
	}
	if err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_supersede_checked_project'`,
	).Scan(&name); err != nil {
		t.Fatalf("idx_supersede_checked_project missing after migrateV8: %v", err)
	}

	// The migrated table is usable and stores the pair.
	if _, err := db.Exec(
		`INSERT INTO supersede_checked (newer_id, older_id, project_id, newer_hash, older_hash) VALUES ('m1', 'm2', 'p1', 'h1', 'h2')`,
	); err != nil {
		t.Fatalf("insert after migrateV8: %v", err)
	}
	var newerHash string
	if err := db.QueryRow(
		`SELECT newer_hash FROM supersede_checked WHERE newer_id = 'm1' AND older_id = 'm2'`,
	).Scan(&newerHash); err != nil {
		t.Fatalf("read migrated table: %v", err)
	}
	if newerHash != "h1" {
		t.Errorf("newer_hash = %q, want h1", newerHash)
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

// TestMigrateV9AddsMaintenanceRuns exercises the one-shot upgrade every existing
// v8 database takes: migrateV9 must create maintenance_runs (schemaVersion 9),
// stamp the version, and leave existing rows untouched. The fixture drops the
// table initSQL would create and stamps user_version=8 to simulate a real
// pre-migration database, and migrate() is called directly — bypassing
// OpenDB's unconditional initSQL run — so the step's own DDL is what the test
// proves.
func TestMigrateV9AddsMaintenanceRuns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open v8 db: %v", err)
	}
	defer db.Close() //nolint:errcheck

	if _, err := db.Exec(initSQL); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE maintenance_runs`); err != nil {
		t.Fatalf("drop maintenance_runs to simulate a v8 database: %v", err)
	}
	seed := []string{
		`INSERT INTO projects (id, path, name) VALUES ('p1', '/tmp/v9-p1', 'p1')`,
		`PRAGMA user_version = 8`,
	}
	for _, s := range seed {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed v8 db (%s): %v", s, err)
		}
	}

	if err := migrate(db, 8); err != nil {
		t.Fatalf("migrate v8->v9: %v", err)
	}
	if v := schemaVersionOf(t, db); v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}

	// The migrated table is usable and stores a budget-check event.
	if _, err := db.Exec(
		`INSERT INTO maintenance_runs (kind, scratch_bytes, scratch_reaped_bytes, scratch_reaped_count, note)
		 VALUES ('scratch-budget', 123456, 8192, 2, 'probe')`,
	); err != nil {
		t.Fatalf("insert after migrateV9: %v", err)
	}
	var gotBytes, gotReaped int
	var gotNote string
	if err := db.QueryRow(
		`SELECT scratch_bytes, scratch_reaped_count, note FROM maintenance_runs WHERE kind='scratch-budget'`,
	).Scan(&gotBytes, &gotReaped, &gotNote); err != nil {
		t.Fatalf("select from maintenance_runs: %v", err)
	}
	if gotBytes != 123456 || gotReaped != 2 || gotNote != "probe" {
		t.Errorf("row = (%d, %d, %q), want (123456, 2, probe)", gotBytes, gotReaped, gotNote)
	}

	// Pre-migration rows survived the additive step.
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM projects`).Scan(&n); err != nil || n != 1 {
		t.Errorf("projects after migration: n=%d err=%v, want 1", n, err)
	}
}

// TestMigrateFreshDBHasMaintenanceRuns: a brand-new database (initSQL path, no
// migration involved) must have maintenance_runs and its recency index from the
// start — guards against the table silently dropping out of initSQL while
// migrateV9 still exists to paper over it on upgraded databases.
func TestMigrateFreshDBHasMaintenanceRuns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close() //nolint:errcheck

	if _, err := db.Exec(`SELECT scratch_bytes, scratch_reaped_bytes, scratch_reaped_count FROM maintenance_runs LIMIT 0`); err != nil {
		t.Fatalf("maintenance_runs columns missing on fresh db: %v", err)
	}
	var idx string
	if err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_maintenance_runs_at'`,
	).Scan(&idx); err != nil {
		t.Fatalf("idx_maintenance_runs_at index missing on fresh db: %v", err)
	}
}

// TestMigrateV17AddsMemoryHistory exercises the one-shot upgrade every
// existing v16 database takes: migrateV17 must create memory_history and
// both of its indexes, stamp the version, and leave existing rows untouched.
// The fixture drops what initSQL would create and stamps user_version=16 to
// simulate a real pre-migration database, and migrate() is called directly —
// bypassing OpenDB's unconditional initSQL run — so the step's own DDL is what
// the test proves.
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
		// The recorded_at index this migration does not create, under either name
		// it has had: no release ever shipped one, but a developer's build
		// between two commits could have made it, and a stale index would make
		// the absence assertion below fail for a reason that has nothing to do
		// with this step. initSQL does not create it either, so leaving it out
		// of the list would have made this test run against a v16 database that
		// never had it, and the absence assertion would have passed for the
		// wrong reason.
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
	// The refusal is a refusal: the dev table and the corpus are both untouched,
	// and the version is not stamped, so the store is exactly as recoverable as it
	// was before the upgrade was attempted.
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
	// The OPEN path is where the cost and the diagnosis live, and the two
	// consequences of putting the check only in the step are both observable:
	//
	//   - OpenDB takes a full VACUUM INTO copy before migrating, and this
	//     condition is permanent, so a check that ran only inside migrateV18
	//     would write a copy of the database on every open of a store that
	//     cannot be opened (and keep three of them);
	//   - the copy is named at one-second granularity and refuses an existing
	//     path, so a second open in the same second — which is exactly what a
	//     session start does, the hook and the MCP server both opening — would
	//     report "refusing to overwrite an existing file" and name neither the
	//     table nor the remedy.
	//
	// So: no copy is written, and the retry says the same actionable thing.
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

// TestMigrateFreshDBHasMemoryHistory: a brand-new database (initSQL path, no
// migration involved) must have memory_history and its indexes from the start
// — guards against the table silently dropping out of initSQL while migrateV17
// still exists to paper over it on upgraded databases.
func TestMigrateFreshDBHasMemoryHistory(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close() //nolint:errcheck

	if _, err := db.Exec(`SELECT memory_id, phase, agent, session_id, content, category,
		importance, resolved_at, source FROM memory_history LIMIT 0`); err != nil {
		t.Fatalf("memory_history columns missing on fresh db: %v", err)
	}
	// One index, deliberately. The per-memory cap ranks by rowid, which the
	// implicit index serves for free, and an as_of read (#647) filters on
	// memory_id — so a standalone recorded_at index would have no reader while
	// every append paid a second b-tree insert to keep it current, on the write
	// path's critical section. Asserted as an absence: an index added back
	// "just in case" is a cost this test should have to be edited to accept.
	for _, name := range []string{"idx_history_memory"} {
		var idx string
		if err := db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='index' AND name=?`, name,
		).Scan(&idx); err != nil {
			t.Errorf("%s index missing on fresh db: %v", name, err)
		}
	}
	var stray int
	if err := db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='index' AND tbl_name='memory_history'
		 AND name NOT LIKE 'sqlite_autoindex%' AND name <> 'idx_history_memory'`,
	).Scan(&stray); err != nil {
		t.Fatalf("count history-table indexes: %v", err)
	}
	if stray != 0 {
		t.Errorf("memory_history carries %d index(es) nothing reads; each is a b-tree insert on every write", stray)
	}
}

// TestMigrateFreshDBHasMemoryProvenance: a brand-new database (initSQL path, no
// migration involved) must have memory_provenance and its index from the start —
// guards against the table silently dropping out of initSQL while migrateV18 still
// exists to paper over it on upgraded databases.
func TestMigrateFreshDBHasMemoryProvenance(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close() //nolint:errcheck

	if _, err := db.Exec(`SELECT id, memory_id, kind, agent, session_id, source_ref,
		confidence, observed_at, verified_at FROM memory_provenance LIMIT 0`); err != nil {
		t.Fatalf("memory_provenance columns missing on fresh db: %v", err)
	}
	var idx string
	if err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_provenance_memory'`,
	).Scan(&idx); err != nil {
		t.Errorf("idx_provenance_memory missing on fresh db: %v", err)
	}
	// One index, asserted as an absence for the reason the history table keeps
	// one: every append pays a b-tree insert per index, inside the write
	// transaction that holds the write lock.
	var stray int
	if err := db.QueryRow(`
		SELECT count(*) FROM sqlite_master WHERE type='index' AND tbl_name='memory_provenance'
		 AND name NOT LIKE 'sqlite_autoindex%' AND name <> 'idx_provenance_memory'`,
	).Scan(&stray); err != nil {
		t.Fatalf("count evidence indexes: %v", err)
	}
	if stray != 0 {
		t.Errorf("memory_provenance carries %d index(es) nothing reads; each is a b-tree insert on every save", stray)
	}
	// The foreign key is the reason a delete takes the evidence with it, so its
	// absence would be invisible until a delete silently left rows behind.
	var fkSQL string
	if err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='memory_provenance'`,
	).Scan(&fkSQL); err != nil {
		t.Fatalf("read memory_provenance DDL: %v", err)
	}
	if !strings.Contains(fkSQL, "REFERENCES memories(id) ON DELETE CASCADE") {
		t.Errorf("memory_provenance.memory_id does not cascade from memories: %s", fkSQL)
	}
}
