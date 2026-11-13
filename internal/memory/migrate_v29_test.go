package memory

import (
	"database/sql"
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
