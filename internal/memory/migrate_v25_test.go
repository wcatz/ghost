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
