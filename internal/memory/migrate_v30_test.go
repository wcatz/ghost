package memory

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestMigrateV29ToV30AddsProjectCreatedIndex(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "v29.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(initSQL); err != nil {
		t.Fatal(err)
	}
	if err := ensurePostMigrationIndexes(db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version = 29"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP INDEX IF EXISTS idx_memories_project_created`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	migrated, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() { _ = migrated.Close() }()

	if v := schemaVersionOf(t, migrated); v != schemaVersion {
		t.Fatalf("user_version = %d, want %d", v, schemaVersion)
	}
	var idxName string
	if err := migrated.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_memories_project_created'`).Scan(&idxName); err != nil {
		t.Fatalf("index missing: %v", err)
	}
	if idxName != "idx_memories_project_created" {
		t.Fatalf("idx name = %q", idxName)
	}
}
