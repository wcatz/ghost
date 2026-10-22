package memory

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// dropSnapshotUpdatedAt strips the v29 column so a database can be left shaped
// as a v28 build left it.
func dropSnapshotUpdatedAt(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`ALTER TABLE memory_snapshots DROP COLUMN updated_at`); err != nil {
		t.Fatalf("drop memory_snapshots.updated_at: %v", err)
	}
}

// TestMigrateV29MatchesTheFreshDatabaseSchema: the migration path and the fresh
// path (initSQL) must produce the same memory_snapshots table.
func TestMigrateV29MatchesTheFreshDatabaseSchema(t *testing.T) {
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
	fresh, migrated := newDB(t), newDB(t)
	dropSnapshotUpdatedAt(t, migrated)
	tx, err := migrated.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateV29(tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migrateV29: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	freshCols, err := columnShapes(t, fresh, "memory_snapshots")
	if err != nil || len(freshCols) == 0 {
		t.Fatalf("a fresh database has no memory_snapshots (%v, %v)", freshCols, err)
	}
	migratedCols, err := columnShapes(t, migrated, "memory_snapshots")
	if err != nil {
		t.Fatal(err)
	}
	if diff := columnShapeDiff(freshCols, migratedCols); diff != "" {
		t.Errorf("the migration path and initSQL disagree on the table: %s", diff)
	}
}

// TestOpenDBUpgradesAV28DatabaseAndItsOldSnapshotReadsNull is the upgrade path
// a real store takes: a v28 database is reopened, migrateV29 adds the column, a
// snapshot row that predates it reads NULL, and a second run is harmless.
func TestOpenDBUpgradesAV28DatabaseAndItsOldSnapshotReadsNull(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v28.db")
	seed, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if _, err := seed.Exec(`INSERT INTO projects (id, path, name) VALUES (?, ?, ?)`,
		testProject, "/tmp/a-v28-store", "v28"); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	const memoryID = "M-v28"
	if _, err := seed.Exec(`INSERT INTO memories (id, project_id, category, content, source, importance)
		VALUES (?, ?, 'fact', 'a fact a v28 snapshot recorded', 'reflection', 0.5)`,
		memoryID, testProject); err != nil {
		t.Fatalf("seed memory: %v", err)
	}
	if _, err := seed.Exec(`INSERT INTO memory_snapshots (snapshot_id, project_id, category, content, importance, source, memory_id, created_at)
		VALUES (?, ?, 'fact', 'a fact a v28 snapshot recorded', 0.5, 'reflection', ?, '2026-01-01 00:00:00')`,
		testProject+"-snapshot", testProject, memoryID); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	dropSnapshotUpdatedAt(t, seed)
	if _, err := seed.Exec(`PRAGMA user_version = 28`); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB on a v28 database: %v", err)
	}
	defer db.Close() //nolint:errcheck
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}
	var updated sql.NullString
	if err := db.QueryRow(`SELECT updated_at FROM memory_snapshots WHERE memory_id = ?`, memoryID).Scan(&updated); err != nil {
		t.Fatalf("read upgraded snapshot row: %v", err)
	}
	if updated.Valid {
		t.Errorf("upgraded snapshot row has updated_at %q, want NULL", updated.String)
	}

	// second run is a no-op
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateV29(tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migrateV29 (second): %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
