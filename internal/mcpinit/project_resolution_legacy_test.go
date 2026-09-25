package mcpinit

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestResolveMarkerProjectReadsPreV11Database(t *testing.T) {
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "ghost.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE projects (
		id TEXT PRIMARY KEY,
		path TEXT NOT NULL UNIQUE,
		name TEXT NOT NULL
	)`); err != nil {
		_ = db.Close()
		t.Fatalf("create legacy projects table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('legacy-id', '/tmp/legacy', 'legacy')`); err != nil {
		_ = db.Close()
		t.Fatalf("insert legacy project: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 10`); err != nil {
		_ = db.Close()
		t.Fatalf("stamp legacy schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	if got := resolveMarkerProject(dataDir, "legacy"); got != "legacy-id" {
		t.Fatalf("resolveMarkerProject(legacy) = %q, want legacy-id", got)
	}
}
