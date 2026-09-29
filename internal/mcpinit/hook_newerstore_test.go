package mcpinit

// The session hook's half of #746, and the reason it is its own file.
//
// internal/memory refuses a write into a store a newer Ghost owns, and the
// structural test that keeps every store write behind a guarded seam walks
// internal/memory. This package is outside that walk and opens its own handle
// with sql.Open, so the session hook's single write was the one write into a
// memory store that nothing checked.
//
// That is not a small gap. The session hook runs on every Claude Code session
// start, which is exactly the moment a stale `ghost mcp` server is still alive
// and still serving the store that a newer binary migrated underneath it.

import (
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// TestBumpSessionCountSkipsAStoreANewerGhostOwns is the behavioural half: the
// hook must leave the counter alone when the store has moved past this build.
//
// The store is opened and closed first, so the stamp below lands on a file no
// handle of ours holds open — which is the shape of the incident. A newer Ghost
// is a separate process, and it is the one that stamps.
func TestBumpSessionCountSkipsAStoreANewerGhostOwns(t *testing.T) {
	dbPath := sessionHookDB(t)
	stampStoreNewer(t, dbPath)

	before := interactionCount(t, dbPath)
	if n := bumpSessionCount(dbPath, "p1"); n != 0 {
		t.Errorf("bumpSessionCount = %d, want 0 — the hook wrote into a store a newer Ghost owns, "+
			"which is the #746 incident arriving through a path the store's own guard cannot see", n)
	}
	if after := interactionCount(t, dbPath); after != before {
		t.Errorf("interaction_count = %d, want it still %d — the hook's write landed despite the version guard", after, before)
	}
}

// TestBumpSessionCountStillWritesAStoreThisBuildOwns is the other half, and it
// is the one that keeps the guard from being "skip if anything looks odd".
//
// A check that refuses on doubt passes its own test and breaks the feature: the
// counter would silently stop advancing on every machine, and nothing would say
// why. The two are adjacent deliberately — same fixture, same hook, one version
// apart.
func TestBumpSessionCountStillWritesAStoreThisBuildOwns(t *testing.T) {
	dbPath := sessionHookDB(t)

	if n := bumpSessionCount(dbPath, "p1"); n != 1 {
		t.Errorf("bumpSessionCount = %d, want 1 — the version guard must not refuse a store at this build's own schema", n)
	}
	if got := interactionCount(t, dbPath); got != 1 {
		t.Errorf("interaction_count = %d, want 1", got)
	}
}

// TestBumpSessionCountCannotBeSlippedPastByAMigrationCommittingMidCheck is the
// race, and it is a test rather than a comment because the whole reason
// internal/memory puts its check AFTER BeginTx is that a pre-BEGIN read leaves a
// window — and this function's first version reproduced that window in two lines.
//
// The sequence is deterministic rather than timed, and it works because of one
// SQLite fact: a WAL reader does NOT block on a writer, and does NOT see its
// uncommitted changes. So:
//
//  1. A second connection takes the write lock and stamps user_version WITHOUT
//     committing. The stamp is now made but invisible.
//  2. bumpSessionCount runs. A pre-BEGIN check is a WAL read: it sees the OLD
//     version, says "fine", and its UPSERT then blocks on the write lock — and
//     lands as soon as the stamp commits. That is the incident, in miniature.
//  3. The stamp commits. A check that runs INSIDE a write transaction cannot
//     have read anything: BEGIN IMMEDIATE could not even have started until the
//     stamp committed, so it reads the NEW version and skips.
//
// It runs as a pair with the SAME-VALUE control below, and the pair is the point:
// the first version of this test asserted only that the hook returned 0, which a
// guard that refuses on ANY doubt also satisfies. It then passed for the wrong
// reason under two separate mutations — drop _txlock=immediate, or read the
// version on the pool — because a deferred transaction whose snapshot went stale
// fails its UPSERT with SQLITE_BUSY_SNAPSHOT and returns 0 too. A guard that
// cannot tell "the store is newer" from "my write failed" is not a guard.
func TestBumpSessionCountCannotBeSlippedPastByAMigrationCommittingMidCheck(t *testing.T) {
	t.Run("a migration committing mid-check refuses the write", func(t *testing.T) {
		dbPath := sessionHookDB(t)
		concurrentCommit(t, dbPath, `PRAGMA user_version = `+strconv.Itoa(memory.SchemaVersion()+1))

		if n := bumpSessionCount(dbPath, "p1"); n != 0 {
			t.Errorf("bumpSessionCount = %d, want 0 — it read the version before the migration committed, "+
				"then wrote after it did. That is the #746 incident arriving through a two-statement "+
				"guard, and the fix is the one internal/memory uses: take the write lock FIRST, then "+
				"read through the transaction", n)
		}
		if after := interactionCount(t, dbPath); after != 0 {
			t.Errorf("interaction_count = %d, want 0 — the hook's write landed despite the migration", after)
		}
	})

	// The control, and the reason the other half exists. An UNRELATED commit
	// landing at the same moment must not stop the counter: the store is at this
	// build's own schema, so the write is legitimate and refusing it would be a
	// guard that fails closed on ordinary concurrency. It is also what catches a
	// transaction that reads the version and then cannot complete its write —
	// SQLITE_BUSY_SNAPSHOT looks exactly like a refusal from the caller's side.
	t.Run("an unrelated commit does not refuse the write", func(t *testing.T) {
		dbPath := sessionHookDB(t)
		// A real write that commits, changing nothing this test asserts on. It
		// invalidates a WAL snapshot, which is the mechanism under test.
		concurrentCommit(t, dbPath, `UPDATE ghost_state SET updated_at = updated_at WHERE project_id = 'p1'`)

		if n := bumpSessionCount(dbPath, "p1"); n != 1 {
			t.Errorf("bumpSessionCount = %d, want 1 — the store is at this build's own schema, so an "+
				"unrelated concurrent commit must not stop the counter. Refusing here means the guard "+
				"cannot tell a newer store from a failed write, which is a refusal on doubt", n)
		}
		if after := interactionCount(t, dbPath); after != 1 {
			t.Errorf("interaction_count = %d, want 1", after)
		}
	})
}

// concurrentCommit takes the write lock on dbPath, applies stmt, and commits it
// after a delay long enough for the hook to have got as far as it can, then
// returns. The stamp is invisible for the whole of that delay.
//
// The delay is what makes this deterministic rather than a race: SQLite's
// busy_timeout(5000) covers the hook's wait comfortably, and 250ms is far longer
// than the hook needs to reach its version read.
func concurrentCommit(t *testing.T, dbPath, stmt string) {
	t.Helper()
	holding := make(chan struct{})
	committed := make(chan struct{})
	go func() {
		defer close(committed)
		db, err := sql.Open("sqlite", rwDSN(dbPath))
		if err != nil {
			t.Errorf("open the blocking handle: %v", err)
			close(holding)
			return
		}
		defer func() { _ = db.Close() }()
		// Force a real connection; sql.Open does no I/O.
		var one int
		if err := db.QueryRow(`SELECT count(*) FROM projects`).Scan(&one); err != nil {
			t.Errorf("blocker query: %v", err)
			close(holding)
			return
		}
		tx, err := db.Begin()
		if err != nil {
			t.Errorf("blocker Begin: %v", err)
			close(holding)
			return
		}
		if _, err := tx.Exec(stmt); err != nil {
			t.Errorf("blocker %q: %v", stmt, err)
			_ = tx.Rollback()
			close(holding)
			return
		}
		close(holding)
		time.Sleep(250 * time.Millisecond)
		if err := tx.Commit(); err != nil {
			t.Errorf("blocker commit: %v", err)
		}
	}()
	<-holding
	t.Cleanup(func() { <-committed })
}

// sessionHookDB returns the path of a data directory holding a closed, migrated
// store with one project, which is what the hook finds on a machine where Ghost
// has been used before.
func sessionHookDB(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "ghost")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	dbPath := filepath.Join(dir, "ghost.db")
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if err := testStore(db).EnsureProject(t.Context(), "p1", "/tmp/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return dbPath
}

// stampStoreNewer moves the file to the next schema version through a SEPARATE
// connection, then closes it. It is what a newer ghost process does, and the
// separate connection is the point: the guard under test must read the stamp
// from the file rather than from anything this process cached earlier.
func stampStoreNewer(t *testing.T, dbPath string) {
	t.Helper()
	db, err := sql.Open("sqlite", rwDSN(dbPath))
	if err != nil {
		t.Fatalf("open the stamp handle: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`PRAGMA user_version = ` + strconv.Itoa(memory.SchemaVersion()+1)); err != nil {
		t.Fatalf("stamp the store newer: %v", err)
	}
}

// interactionCount reads the counter the hook maintains, through its own
// read-only handle so the test never writes the row it is asserting about.
func interactionCount(t *testing.T, dbPath string) int {
	t.Helper()
	db, err := memory.OpenReadDB(dbPath)
	if err != nil {
		t.Fatalf("OpenReadDB: %v", err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow(`SELECT interaction_count FROM ghost_state WHERE project_id = 'p1'`).Scan(&n); err != nil {
		t.Fatalf("read interaction_count: %v", err)
	}
	return n
}
