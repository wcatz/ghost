package memory

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func columnNames(t *testing.T, db *sql.DB, table string) map[string]bool {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatalf("read %s columns: %v", table, err)
	}
	defer func() { _ = rows.Close() }()

	got := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan column info: %v", err)
		}
		got[name] = true
	}
	return got
}

// TestMigrateV10AddsProvenanceAndValidity upgrades a schema-v9 database —
// the exact memories DDL that shipped through v9 — and asserts every new
// column arrives.
//
// The pre-existing row is the point of the test, not decoration: an additive
// migration must leave existing memories untouched with their new columns
// reading NULL, because NULL is the honest record for provenance Ghost never
// observed. A migration that stamped a default instead would be asserting a
// provenance fact it cannot know.
func TestMigrateV10AddsProvenanceAndValidity(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open v9 db: %v", err)
	}
	defer func() { _ = db.Close() }()

	v9 := []string{
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
		`INSERT INTO projects (id, path, name) VALUES ('p1', '/tmp/v10-p1', 'p1')`,
		`INSERT INTO memories (id, project_id, content, resolved_at)
		  VALUES ('m1', 'p1', 'a note written before provenance existed', '2026-01-02 03:04:05')`,
		`PRAGMA user_version = 9`,
	}
	for _, s := range v9 {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed v9 db: %v", err)
		}
	}

	if err := migrate(db, 9); err != nil {
		t.Fatalf("migrate v9->v10: %v", err)
	}
	if v := schemaVersionOf(t, db); v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}

	cols := columnNames(t, db, "memories")
	for _, c := range phase1aProvenanceColumns {
		if !cols[c.name] {
			t.Errorf("memories.%s missing after migrateV10", c.name)
		}
	}

	// The pre-existing row survives with its original values intact, and its
	// new provenance columns read NULL rather than a fabricated default.
	var content, resolvedAt string
	var agent, sessionID, sourceRef sql.NullString
	var validFrom, validUntil, verifiedAt sql.NullString
	var confidence sql.NullFloat64
	err = db.QueryRow(`SELECT content, resolved_at,
	        agent, session_id, source_ref, confidence,
	        valid_from, valid_until, verified_at
	     FROM memories WHERE id = 'm1'`).
		Scan(&content, &resolvedAt,
			&agent, &sessionID, &sourceRef, &confidence,
			&validFrom, &validUntil, &verifiedAt)
	if err != nil {
		t.Fatalf("read migrated row: %v", err)
	}
	if content != "a note written before provenance existed" {
		t.Errorf("content = %q, want the pre-migration text", content)
	}
	if resolvedAt != "2026-01-02 03:04:05" {
		t.Errorf("resolved_at = %q, want the pre-migration stamp", resolvedAt)
	}
	for name, v := range map[string]bool{
		"agent": agent.Valid, "session_id": sessionID.Valid, "source_ref": sourceRef.Valid,
		"valid_from": validFrom.Valid, "valid_until": validUntil.Valid, "verified_at": verifiedAt.Valid,
	} {
		if v {
			t.Errorf("%s was populated by the migration; provenance Ghost never observed must stay NULL", name)
		}
	}
	if confidence.Valid {
		t.Errorf("confidence = %v, want NULL — a guessed confidence asserts a trustworthiness this row never had", confidence.Float64)
	}
}

// TestMigrateFreshDBHasProvenanceAndValidity: a brand-new database takes the
// initSQL path and never runs migrate(), so it needs the columns from the
// start. Guards against the columns existing only in migrateV10 while
// initSQL drifts, which would give fresh installs a schema that differs from
// upgraded ones.
func TestMigrateFreshDBHasProvenanceAndValidity(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "ghost.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() { _ = db.Close() }()

	cols := columnNames(t, db, "memories")
	for _, c := range phase1aProvenanceColumns {
		if !cols[c.name] {
			t.Errorf("memories.%s missing on a fresh database (initSQL)", c.name)
		}
	}

	// Every new column must accept a real write, so a caller that records
	// provenance at save time does not discover the DDL is unusable.
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('p1', '/tmp/fresh-p1', 'p1')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	_, err = db.Exec(`INSERT INTO memories
	    (id, project_id, content, agent, session_id, source_ref, confidence,
	     valid_from, valid_until, verified_at)
	    VALUES ('m1', 'p1', 'observed fact', 'opencode', 'ses_abc',
	            'helmfile.yaml:L20', 0.9,
	            '2026-09-24', NULL, '2026-09-24')`)
	if err != nil {
		t.Fatalf("insert with provenance and validity: %v", err)
	}
	var confidence float64
	if err := db.QueryRow(`SELECT confidence FROM memories WHERE id='m1'`).Scan(&confidence); err != nil {
		t.Fatalf("read confidence back: %v", err)
	}
	if confidence != 0.9 {
		t.Errorf("confidence = %v, want 0.9", confidence)
	}
}
