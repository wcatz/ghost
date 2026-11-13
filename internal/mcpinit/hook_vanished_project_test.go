package mcpinit

// The session hook's write against a project that is gone (#1155).
//
// The hook resolves a project id early and bumps that project's counter late.
// Between the two, the project can be deleted (`ghost project delete`, the
// ghost_project_delete tool from another session, a duplicate-project fold). A
// write connection without foreign_keys accepted the counter row anyway, and the
// orphan it left made the next upgrade's migration refuse to open the store.

import (
	"database/sql"
	"testing"
)

// TestRwDSNEnforcesForeignKeys reads the setting back from a live connection
// rather than from the DSN string, the way TestMultiProcessSharedDatabase does
// for memory.OpenDB — which never opens this DSN, so nothing else would notice
// it going missing.
func TestRwDSNEnforcesForeignKeys(t *testing.T) {
	dbPath := sessionHookDB(t)
	db, err := sql.Open("sqlite", rwDSN(dbPath))
	if err != nil {
		t.Fatalf("open rwDSN: %v", err)
	}
	defer func() { _ = db.Close() }()
	var fk int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatalf("read foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("PRAGMA foreign_keys on an rwDSN connection = %d, want 1 — every writer to the store enforces foreign keys, and this one is the session hook's", fk)
	}
}
