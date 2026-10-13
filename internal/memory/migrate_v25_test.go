package memory

// #1053's schema: the validity arm of the vector epoch, a frozen migration step.
//
// The vector leg reads a memory's valid_from/valid_until to withhold an
// out-of-window row before the result window closes, so a cached corpus holds
// those bounds (#1031's cache) and must be dropped the moment an edit changes
// one. The counter already bumps on the other columns the vector query reads;
// this step adds the two it now reads too.

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// assertValidityEpochTriggerFires pins the one behaviour the step exists for,
// against a database that already holds M1: a validity-window change bumps the
// counter, and an update that changes nothing does not.
func assertValidityEpochTriggerFires(t *testing.T, db *sql.DB) {
	t.Helper()
	before := rawEpoch(t, db)
	if _, err := db.Exec(`UPDATE memories SET valid_until = datetime('now', '+1 day') WHERE id = 'M1'`); err != nil {
		t.Fatalf("validity update: %v", err)
	}
	if after := rawEpoch(t, db); after <= before {
		t.Errorf("a validity-window update left vector_epoch at %d (was %d): the cache would serve stale bounds", after, before)
	}
	before = rawEpoch(t, db)
	if _, err := db.Exec(`UPDATE memories SET valid_until = valid_until WHERE id = 'M1'`); err != nil {
		t.Fatalf("no-op validity update: %v", err)
	}
	if after := rawEpoch(t, db); after != before {
		t.Errorf("a validity update that changed nothing moved vector_epoch from %d to %d", before, after)
	}
}

// TestMigrateV25MatchesTheFreshDatabaseSchema: the migration path and the fresh
// path (initSQL, then vectorEpochSQL) produce the same validity trigger, or a
// store that reached v25 by upgrading holds a different counter than one created
// fresh at the same version.
//
// The step is RUN DIRECTLY rather than through OpenDB, for the reason
// TestMigrateV24MatchesTheFreshDatabaseSchema states: OpenDB runs initSQL first,
// which would put the trigger back before the step could be seen to do it.
func TestMigrateV25MatchesTheFreshDatabaseSchema(t *testing.T) {
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
		if _, err := db.Exec(vectorEpochSQL); err != nil {
			t.Fatalf("vectorEpochSQL: %v", err)
		}
		return db
	}
	fresh, migrated := newDB(t), newDB(t)
	// Strip only the v25 object, so the step has work to do and the rest of the
	// counter is left in place as a database upgrading from v24 would have it.
	if _, err := migrated.Exec(`DROP TRIGGER IF EXISTS vector_epoch_mem_validity_au`); err != nil {
		t.Fatalf("drop the validity trigger: %v", err)
	}
	tx, err := migrated.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateV25(tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migrateV25: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	if f, m := triggerSQL(t, fresh, "vector_epoch_mem_validity_au"), triggerSQL(t, migrated, "vector_epoch_mem_validity_au"); f != m {
		t.Errorf("the validity trigger differs between the paths:\n fresh    %s\n migrated %s", f, m)
	}
	// The migrated database's trigger does the work, not only exists.
	seedOneEmbeddedMemory(t, migrated, true)
	assertValidityEpochTriggerFires(t, migrated)
}
