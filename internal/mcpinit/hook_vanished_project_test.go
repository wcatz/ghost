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
	"errors"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// deleteProjectP1 removes the fixture's project through the store's own handle,
// which is what another process's delete looks like to the hook.
func deleteProjectP1(t *testing.T, dbPath string) {
	t.Helper()
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`DELETE FROM projects WHERE id = 'p1'`); err != nil {
		t.Fatalf("delete project p1: %v", err)
	}
}

// ghostStateRows counts the counter rows for projectID, through a read-only
// handle so the count is not taken on a connection the test wrote with.
func ghostStateRows(t *testing.T, dbPath, projectID string) int {
	t.Helper()
	db, err := memory.OpenReadDB(dbPath)
	if err != nil {
		t.Fatalf("OpenReadDB: %v", err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ghost_state WHERE project_id = ?`, projectID).Scan(&n); err != nil {
		t.Fatalf("count ghost_state rows: %v", err)
	}
	return n
}

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

// TestBumpSessionCountForAVanishedProjectCountsNothing is the reproduction from
// the issue: the project goes, then the hook bumps it. Nothing is counted, no
// orphan is left, and the hook's write stays silent — it returns 0, which the
// caller reads as "show the count you already have".
func TestBumpSessionCountForAVanishedProjectCountsNothing(t *testing.T) {
	dbPath := sessionHookDB(t)

	// A second project that stays, so the same store shows an ordinary bump
	// still counting after the vanished one is refused.
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if err := testStore(db).EnsureProject(t.Context(), "p2", "/tmp/p2", "p2"); err != nil {
		t.Fatalf("EnsureProject p2: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	deleteProjectP1(t, dbPath)

	if n := bumpSessionCount(dbPath, "p1"); n != 0 {
		t.Errorf("bumpSessionCount for a deleted project = %d, want 0 — there is no project to count a session for", n)
	}
	if n := ghostStateRows(t, dbPath, "p1"); n != 0 {
		t.Errorf("ghost_state rows for the deleted project = %d, want 0 — an orphan here makes the next migration refuse the store", n)
	}

	if n := bumpSessionCount(dbPath, "p2"); n != 1 {
		t.Errorf("bumpSessionCount for a live project = %d, want 1", n)
	}
	if n := bumpSessionCount(dbPath, "p2"); n != 2 {
		t.Errorf("second bumpSessionCount for a live project = %d, want 2", n)
	}
}

// TestBumpSessionCountStatementSkipsAVanishedProjectWithoutForeignKeys pins the
// statement's own guard, separately from the DSN: on a connection that does NOT
// enforce foreign keys (an older build's DSN, or this one regressing), the
// statement still writes nothing for a project that is gone. With the guard
// removed, the foreign-key setting would be the only thing standing between the
// hook and an orphan row, and this test is what notices.
func TestBumpSessionCountStatementSkipsAVanishedProjectWithoutForeignKeys(t *testing.T) {
	dbPath := sessionHookDB(t)
	deleteProjectP1(t, dbPath)

	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=foreign_keys(OFF)")
	if err != nil {
		t.Fatalf("open without foreign keys: %v", err)
	}
	defer func() { _ = db.Close() }()
	var fk int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 0 {
		t.Fatalf("PRAGMA foreign_keys = %d (%v), want 0 for this test to mean anything", fk, err)
	}

	var n int
	err = db.QueryRow(bumpSessionCountSQL, "p1").Scan(&n)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("bump statement for a deleted project: n=%d err=%v, want sql.ErrNoRows", n, err)
	}
	if n := ghostStateRows(t, dbPath, "p1"); n != 0 {
		t.Errorf("ghost_state rows for the deleted project = %d, want 0", n)
	}
}
