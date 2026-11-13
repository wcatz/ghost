package memory

// The write-side half of #746: a store this build opened as vN refuses to
// write once ANOTHER handle has stamped the file N+1, and reads keep working.
//
// The incident the issue reports is an ORDERING fact, not a version fact: a
// running `ghost mcp` opened the store at v17, a newer binary migrated it to
// v18 underneath it, and the old server kept writing for ~40 hours — 32 saves
// with no history row, an edit with no version, deletes with no tombstone. The
// check OpenDB already does was correct and useless here, because OpenDB ran
// once, hours before the migration that invalidated its answer.
//
// So the test drives the real ordering: open at vN, bump the stamp on a SECOND
// connection (which is what a newer ghost process does — it migrates through
// its own handle), then write.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
)

// fileBackedStore opens a store on a real file, which :memory: cannot express:
// PRAGMA user_version is state of a SHARED file read through a connection, and
// the whole check turns on a second connection seeing the first one's commit. A
// :memory: database is private to its connection, so a second handle would open
// a second, empty database and the test would pass against nothing.
func fileBackedStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ghost.db")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil))), path
}

// stampFromAnotherHandle is what a newer ghost process does to the store
// underneath a running server: it stamps the file forward through its own
// connection. It opens a SEPARATE *sql.DB rather than reusing the store's,
// because that is the thing under test — the refusal has to come from another
// connection's COMMIT, not from this process noticing its own write. (Not the
// package's bumpUserVersion, which stamps a snapshot file and is about restore
// refusals rather than a live server.)
func stampFromAnotherHandle(t *testing.T, path string, to int) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open second handle: %v", err)
	}
	defer db.Close() //nolint:errcheck
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", to)); err != nil {
		t.Fatalf("bump user_version to %d: %v", to, err)
	}
}

// The single-statement writes are the second half of the same hole, and the half
// a check placed in beginWrite misses entirely: they had no transaction to place
// a check in, so a running server went straight through them for as long as the
// store was left alone. Each one is now its own guarded transaction, and each is
// named here so the reroute is pinned by behaviour rather than by the structural
// scan alone.
func TestEveryAutocommitWriteIsRefusedAfterAnotherHandleStampsTheStoreNewer(t *testing.T) {
	s, path := fileBackedStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "abc123", "/tmp/test", "test-project"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	id, err := s.Create(ctx, "abc123", Memory{
		Category: "fact", Content: "the row every autocommit write below touches", Source: "mcp", Importance: 0.5,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	otherID, err := s.Create(ctx, "abc123", Memory{
		Category: "fact", Content: "a second row, for the link and the supersede", Source: "mcp", Importance: 0.5,
	})
	if err != nil {
		t.Fatalf("Create second: %v", err)
	}
	taskID, err := s.CreateTask(ctx, "abc123", "a task", "so CompleteTask has a row", 2)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	staleDecision, _, _, err := s.RecordDecision(ctx, "abc123", "stale", "the old call", "because", nil, nil)
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	freshDecision, _, _, err := s.RecordDecision(ctx, "abc123", "fresh", "the new call", "because", nil, nil)
	if err != nil {
		t.Fatalf("RecordDecision second: %v", err)
	}
	if err := s.CreateLink(ctx, id, otherID, "related", 0.5, "manual"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}

	stampFromAnotherHandle(t, path, SchemaVersion()+1)

	writes := []struct {
		op  string
		run func() error
	}{
		{"toggle-pin", func() error { return s.TogglePin(ctx, id, true) }},
		{"touch", func() error { return s.Touch(ctx, []string{id}) }},
		{"mark-link-scanned", func() error { return s.MarkLinkScanned(ctx, id, nil) }},
		{"invalidate-link", func() error {
			_, err := s.InvalidateLink(ctx, id, otherID, "related")
			return err
		}},
		{"store-embedding", func() error { return s.StoreEmbedding(ctx, id, []float32{0.1, 0.2}, "test-model") }},
		{"delete-embedding", func() error { return s.DeleteEmbedding(ctx, id) }},
		{"complete-task", func() error {
			_, err := s.CompleteTask(ctx, taskID, "done before the migration landed")
			return err
		}},
		{"update-task", func() error {
			status := "done"
			_, err := s.UpdateTask(ctx, taskID, &status, nil, nil)
			return err
		}},
		// The two RETURNING writes. They are here because they are the shape a
		// name-based scan MISSES: both reach SQLite through QueryRowContext
		// rather than ExecContext, so neither a transaction nor an autocommit
		// seam can be what stops them, and the structural test that is supposed
		// to hold the invariant would not have flagged either. CreateTask is
		// live — the ghost_task_create tool calls it.
		{"create-task", func() error {
			_, err := s.CreateTask(ctx, "abc123", "written by a stale server", "", 2)
			return err
		}},
		{"increment-interaction", func() error {
			_, err := s.IncrementInteraction(ctx, "abc123")
			return err
		}},
		{"supersede-decision", func() error {
			return s.SupersedeDecision(ctx, "abc123", staleDecision, freshDecision)
		}},
		{"update-learned-context", func() error {
			return s.UpdateLearnedContext(ctx, "abc123", "a summary from a stale server", "and a reflection")
		}},
		{"set-reflect-signature", func() error {
			return s.SetReflectInputSignature(ctx, "abc123", "0123456789abcdef")
		}},
		{"seed-global-memories", func() error { return s.SeedGlobalMemories(ctx) }},
	}
	for _, w := range writes {
		t.Run(w.op+" is refused", func(t *testing.T) {
			err := w.run()
			if err == nil {
				t.Fatal("write succeeded against a store stamped newer than this build — the running server would keep writing")
			}
			if !errors.Is(err, ErrStoreNewer) {
				t.Fatalf("error = %v, want it to wrap ErrStoreNewer", err)
			}
		})
	}

	// The refusals must be REFRESHED per write, not remembered from the first
	// one: a guard that latched after the first refusal would let everything
	// after it through, which is the shape of failure the removed cache had.
	// A second Create is the probe — it is a different path from any above and
	// it must still refuse after eleven refusals in a row.
	if _, err := s.Create(ctx, "abc123", Memory{
		Category: "fact", Content: "and again", Source: "mcp", Importance: 0.5,
	}); !errors.Is(err, ErrStoreNewer) {
		t.Fatalf("write after eleven refusals = %v, want ErrStoreNewer — the check must be read per write, not latched", err)
	}
}

// The check runs on EVERY write, and the cost is one PRAGMA on a page the open
// transaction already holds.
//
// This replaced a cache keyed on PRAGMA data_version, which was wrong: that
// pragma is a per-CONNECTION counter, not a file property, so a cache cannot
// tell a stale value from a fresh one on a replacement connection. The test that
// pinned the cache is gone with it; what replaces it pins the property the
// cache was supposed to buy and could not — that a fresh read is correct even
// when the connection underneath has been swapped for one reporting the same
// counter.
func TestStoreNewerCheckReadsUserVersionOnEveryWrite(t *testing.T) {
	s, path := fileBackedStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "abc123", "/tmp/test", "test-project"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	restore := countStoreVersionProbes(t)
	defer restore()

	const writes = 3
	for i := range writes {
		if _, err := s.Create(ctx, "abc123", Memory{
			Category: "fact", Content: fmt.Sprintf("write %d", i), Source: "mcp", Importance: 0.5,
		}); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}
	// One read per write, no more and no less: a count that DROPS would mean a
	// cache crept back in, which is the defect this test exists to prevent.
	if got := storeVersionProbeCounts()["user_version"]; got != writes {
		t.Errorf("PRAGMA user_version read %d times over %d writes, want one per write — a cache would be "+
			"unsound here, because data_version is a per-connection counter", got, writes)
	}

	// And the read is what makes the refusal correct after a foreign commit,
	// which is the whole point of doing it every time.
	stampFromAnotherHandle(t, path, SchemaVersion()+1)
	if _, err := s.Create(ctx, "abc123", Memory{
		Category: "fact", Content: "after a foreign commit", Source: "mcp", Importance: 0.5,
	}); !errors.Is(err, ErrStoreNewer) {
		t.Fatalf("write after another handle committed = %v, want ErrStoreNewer", err)
	}
}

// A check that cannot answer must not answer "fine", and must not claim the
// store is newer either.
//
// The failing Queryer is deliberate rather than a broken file. A file that is
// not a database, and a closed handle, are both refused EARLIER by BEGIN — the
// good outcome, and one that would leave this branch unpinned because the probe
// is never reached. Injecting the failure at the Queryer is what makes the
// branch testable at all, and it is the state a driver in an unexpected
// condition would produce.
func TestAnUnanswerableVersionProbeRefusesTheWriteWithoutClaimingItIsNewer(t *testing.T) {
	s := testStore(t)
	err := s.checkStoreNotNewer(context.Background(), failingQueryer{})
	if err == nil {
		t.Fatal("check accepted a store whose version it could not read — a write it cannot vouch for must be refused")
	}
	if errors.Is(err, ErrStoreNewer) {
		t.Errorf("error = %v, want a probe failure rather than ErrStoreNewer — an unreadable store is not known to be newer, "+
			"and saying so sends an operator looking for a Ghost build that may not exist", err)
	}
	if !strings.Contains(err.Error(), "user_version") {
		t.Errorf("error = %q, want it to name the probe that failed", err)
	}
}

// failingQueryer refuses every statement, standing in for a driver that cannot
// serve a pragma.
type failingQueryer struct{}

// QueryContext exists to satisfy Queryer and is never called by the check: the
// probe goes through pragmaInt, which uses QueryRowContext. It is written to
// return the same refusal rather than a nil rows, so that a future probe which
// DID use it would fail loudly instead of handing back a nil the caller
// dereferences.
func (failingQueryer) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	return nil, errors.New("probe unavailable")
}

func (failingQueryer) QueryRowContext(_ context.Context, query string, _ ...any) *sql.Row {
	// *sql.Row carries its error privately and cannot be built from outside
	// database/sql, so the refusal is produced the only way a caller can: a
	// query against a CLOSED handle, whose Row fails at Scan with
	// "sql: database is closed". The statement text is accepted and discarded
	// because the error names the handle, not the statement.
	_ = query
	return closedHandle.QueryRow("SELECT 1")
}

// closedHandle is a handle that is already closed, used only to mint the *sql.Row
// that fails. It is opened once because sql.Open does no I/O, and Close is what
// makes every later statement fail.
var closedHandle = func() *sql.DB {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		panic("open the closed-handle fixture: " + err.Error())
	}
	_ = db.Close()
	return db
}()
