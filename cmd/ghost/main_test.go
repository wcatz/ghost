package main

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestRODSNIsReadOnly guards the obsidian commands' read-only guarantee:
// modernc.org/sqlite honors mode=ro only on file: URI DSNs — with a bare
// path the connection opens silently read-write (verified empirically
// against v1.53.0), which is exactly the regression this test would catch.
func TestRODSNIsReadOnly(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	seed, err := memory.OpenDB(dbPath) // creates the schema read-write
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", roDSN(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck

	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('x', '/x', 'x')`); err == nil {
		t.Fatal("write through the read-only DSN must fail")
	}
	// Reads must still work.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM projects`).Scan(&n); err != nil {
		t.Fatalf("read through the read-only DSN must work: %v", err)
	}
}
