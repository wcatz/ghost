package memory

// #1341's schema: memory_snapshots gains the retention tier it captured and the
// marker that says whether it recorded one, delivered by a frozen migration
// step.
//
// The tier is scope's pair one version later. A restore that could not put it
// back turned a restored session row into a project row with no expiry — one
// `ghost prune` could never reach — and let a tier raise the replace's own reuse
// branch had made (raiseReusedRetentionTx) survive the undo. migrateV26 adds the
// two columns and writes no row: the snapshot rows that already exist predate
// the column, so their marker stays 0 and a restore leaves the live tier alone
// rather than believing a tier nobody recorded.

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// dropSnapshotRetention strips the v26 columns so a database can be left shaped
// as a v25 build left it. It is migrateV26's two ALTERs in reverse, and in the
// reverse order: retention_captured first, because it is the NOT NULL one.
func dropSnapshotRetention(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, col := range []string{"retention_captured", "retention"} {
		if _, err := db.Exec(`ALTER TABLE memory_snapshots DROP COLUMN ` + col); err != nil {
			t.Fatalf("drop memory_snapshots.%s: %v", col, err)
		}
	}
}

// TestMigrateV26MatchesTheFreshDatabaseSchema: the migration path and the fresh
// path (initSQL) must produce the same memory_snapshots table, or a store that
// reached v26 by upgrading holds a different shape from one created fresh at the
// same version.
//
// The step is RUN DIRECTLY rather than through OpenDB, for the reason
// TestMigrateV24MatchesTheFreshDatabaseSchema states: OpenDB runs initSQL first,
// which would put the columns back before the step could be seen to add them.
func TestMigrateV26MatchesTheFreshDatabaseSchema(t *testing.T) {
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
	// Strip the v29 column from the fresh side too, so a store at v26 is not
	// compared against a column it does not yet carry.
	dropSnapshotUpdatedAt(t, fresh)
	// Strip the v26 columns, so the step has work to do and the rest of the
	// table is left in place as a database upgrading from v25 would have it.
	dropSnapshotUpdatedAt(t, migrated)
	dropSnapshotRetention(t, migrated)
	tx, err := migrated.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateV26(tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migrateV26: %v", err)
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

// TestOpenDBUpgradesAV25DatabaseAndItsOldSnapshotReadsUncaptured is the upgrade
// path a real store takes, end to end: a database shaped as v25 left it is
// reopened, migrateV26 adds the columns, the snapshot rows that predate them
// read as uncaptured, and a restore of such a snapshot leaves the live tier and
// expiry alone while reporting that it could not put a tier back.
//
// The last part matters as much as the columns: the marker is only worth adding
// if the restore path reads it, and a row written before the column is exactly
// the input that path must not guess about.
func TestOpenDBUpgradesAV25DatabaseAndItsOldSnapshotReadsUncaptured(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v25.db")
	seed, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	// A project, a session memory, and the snapshot row a v25 build would have
	// written for it: no tier column anywhere, because there was none.
	if _, err := seed.Exec(`INSERT INTO projects (id, path, name) VALUES (?, ?, ?)`,
		testProject, "/tmp/a-v25-store", "v25"); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	const memoryID = "M-v25"
	if _, err := seed.Exec(`INSERT INTO memories (id, project_id, category, content, source, importance, retention, expires_at)
		VALUES (?, ?, 'fact', 'a session fact a v25 snapshot recorded', 'reflection', 0.5, 'session', '2030-01-01 00:00:00')`,
		memoryID, testProject); err != nil {
		t.Fatalf("seed memory: %v", err)
	}
	if _, err := seed.Exec(`INSERT INTO memory_snapshots (snapshot_id, project_id, category, content, importance, source, memory_id)
		VALUES (?, ?, 'fact', 'a session fact a v25 snapshot recorded', 0.5, 'reflection', ?)`,
		testProject+"-snapshot", testProject, memoryID); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	dropSnapshotRetention(t, seed)
	if _, err := seed.Exec(`PRAGMA user_version = 25`); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB on a v25 database: %v", err)
	}
	defer db.Close() //nolint:errcheck
	if v := schemaVersionOf(t, db); v != schemaVersion || v < 26 {
		t.Errorf("user_version = %d, want %d (at least 26)", v, schemaVersion)
	}
	// The row the v25 database held is still there, and reads as what it is:
	// a snapshot that recorded no tier.
	var retention sql.NullString
	var captured int
	if err := db.QueryRow(`SELECT retention, retention_captured FROM memory_snapshots WHERE memory_id = ?`, memoryID).
		Scan(&retention, &captured); err != nil {
		t.Fatalf("read the upgraded snapshot row: %v", err)
	}
	if retention.Valid {
		t.Errorf("the upgraded snapshot row claims retention %q; it predates the column and must read as no tier", retention.String)
	}
	if captured != 0 {
		t.Errorf("the upgraded snapshot row has retention_captured = %d, want 0: the step backfills nothing, because a tier nobody recorded cannot be invented", captured)
	}

	// And the restore path reads the marker: the session tier the live row holds
	// stays as it stands, expiry included, and the result says the undo was
	// incomplete.
	store := NewStore(db, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	ctx := context.Background()
	res, err := store.RestoreSnapshot(ctx, testProject)
	if err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	if res.UncapturedRetention == 0 {
		t.Errorf("the restore reported no uncaptured retention for a snapshot whose row predates the column")
	}
	after := getOne(t, store, memoryID)
	if after.Retention != RetentionSession {
		t.Errorf("the restore set the live tier to %q, want %q: a snapshot that recorded no tier must leave the live one alone",
			after.Retention, RetentionSession)
	}
	if after.ExpiresAt == nil || *after.ExpiresAt != "2030-01-01 00:00:00" {
		t.Errorf("the restore touched the live expiry (%s); a snapshot that recorded no tier says nothing about the expiry either",
			expiryText(after.ExpiresAt))
	}
}
