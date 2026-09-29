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
	"strconv"
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

func TestWriteIsRefusedAfterAnotherHandleStampsTheStoreNewer(t *testing.T) {
	s, path := fileBackedStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "abc123", "/tmp/test", "test-project"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	// A write at the store's own version is the control: it proves the refusal
	// below is about the bump and not about the write path.
	if _, err := s.Create(ctx, "abc123", Memory{
		Category: "fact", Content: "written before the migration", Source: "mcp", Importance: 0.5,
	}); err != nil {
		t.Fatalf("Create before bump: %v", err)
	}

	// An expired session row, so the prune-apply case below reaches a write
	// transaction. A prune with no candidates is a read, and a read into a store
	// this build cannot vouch for is allowed on purpose. It goes in its own
	// project so the read assertions below still count only abc123's row.
	if err := s.EnsureProject(ctx, "prn456", "/tmp/prune", "prune-project"); err != nil {
		t.Fatalf("EnsureProject(prune): %v", err)
	}
	if _, _, _, err := s.UpsertWithOptions(ctx, "prn456", "fact", "a session row the prune will want", "mcp", 0.5, nil,
		UpsertOptions{Retention: RetentionSession}); err != nil {
		t.Fatalf("UpsertWithOptions(session): %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE memories SET expires_at = datetime('now', '-10 days'), last_accessed = datetime('now', '-10 days')
		 WHERE project_id = 'prn456' AND retention = 'session'`); err != nil {
		t.Fatalf("age the session row: %v", err)
	}

	stampFromAnotherHandle(t, path, SchemaVersion()+1)

	// Every write path, not just the one beginWrite happens to serve.
	// insertMemory is ghost_memory_create's own transaction and is NOT routed
	// through beginWrite, so a check placed only there would let the issue's
	// own reproduction — a save through the running server — straight through.
	writes := []struct {
		op  string
		run func() error
	}{
		{"create", func() error {
			_, err := s.Create(ctx, "abc123", Memory{
				Category: "fact", Content: "written after the migration", Source: "mcp", Importance: 0.5,
			})
			return err
		}},
		{"create-with-id", func() error {
			_, err := s.CreateWithID(ctx, "abc123", "0123456789ABCDEF0123456789ABCDEF", Memory{
				Category: "fact", Content: "written with an explicit id", Source: "mcp", Importance: 0.5,
			})
			return err
		}},
		{"upsert", func() error {
			_, _, _, err := s.Upsert(ctx, "abc123", "fact", "upserted after the migration", "mcp", 0.5, nil)
			return err
		}},
		{"update", func() error {
			all, err := s.GetAll(ctx, "abc123", 10)
			if err != nil || len(all) == 0 {
				return fmt.Errorf("GetAll: %v (%d rows)", err, len(all))
			}
			content := "edited after the migration"
			return s.UpdateMemory(ctx, "abc123", all[0].ID, &content, nil, nil, nil)
		}},
		{"delete", func() error {
			all, err := s.GetAll(ctx, "abc123", 10)
			if err != nil || len(all) == 0 {
				return fmt.Errorf("GetAll: %v (%d rows)", err, len(all))
			}
			return s.DeleteWithOptions(ctx, all[len(all)-1].ID, DeleteOptions{})
		}},
		{"ensure-project", func() error {
			return s.EnsureProject(ctx, "def456", "/tmp/other", "other-project")
		}},
		{"prune-apply", func() error {
			// A bulk delete, so the dangerous one: it removes rows by a
			// predicate rather than by an id the caller names, and the store
			// would have no record to point at afterwards. The row below is what
			// makes it reach a write transaction at all — a prune with no
			// candidates is a pure read and is refused by nothing.
			_, err := s.PruneSessionMemories(ctx, PruneOptions{Apply: true})
			return err
		}},
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

	// Reads keep working, and that is the point: the agent must still be able
	// to SEARCH, which is what makes a refusal an instruction to restart rather
	// than a dead server.
	t.Run("reads still work", func(t *testing.T) {
		all, err := s.GetAll(ctx, "abc123", 10)
		if err != nil {
			t.Fatalf("GetAll after refusal: %v", err)
		}
		if len(all) != 1 {
			t.Fatalf("got %d memories, want the 1 written before the bump", len(all))
		}
		if all[0].Content != "written before the migration" {
			t.Errorf("content = %q, want the pre-bump text — a refused write must leave no trace", all[0].Content)
		}
	})
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
		{"mark-link-scanned", func() error { return s.MarkLinkScanned(ctx, id) }},
		{"invalidate-link", func() error {
			_, err := s.InvalidateLink(ctx, id, otherID, "related")
			return err
		}},
		{"store-embedding", func() error { return s.StoreEmbedding(ctx, id, []float32{0.1, 0.2}, "test-model") }},
		{"delete-embedding", func() error { return s.DeleteEmbedding(ctx, id) }},
		{"complete-task", func() error { return s.CompleteTask(ctx, taskID, "done before the migration landed") }},
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

func TestStoreNewerErrorNamesBothVersionsAndTheRestart(t *testing.T) {
	err := &StoreNewerError{StoreVersion: SchemaVersion() + 2, BuildVersion: SchemaVersion()}
	msg := err.Error()
	// Both versions, or the operator cannot tell which side moved: the whole
	// diagnosis is "the store moved, not me".
	if !strings.Contains(msg, strconv.Itoa(SchemaVersion()+2)) {
		t.Errorf("error %q does not name the store's version %d", msg, SchemaVersion()+2)
	}
	if !strings.Contains(msg, strconv.Itoa(SchemaVersion())) {
		t.Errorf("error %q does not name this build's version %d", msg, SchemaVersion())
	}
	// The remedy, in the words an operator needs. "Upgrade ghost" is what the
	// OPEN refusal says, and it is wrong here: the binary on disk may already
	// be the new one, and the RUNNING server is the stale half. An operator
	// told to upgrade runs `ghost upgrade`, watches it report "already up to
	// date", and learns nothing.
	if !strings.Contains(msg, "restart the client that runs this ghost server") {
		t.Errorf("error %q does not tell the operator to restart the client running the server", msg)
	}
	// Reads are named as still working, so the refusal reads as "restart" rather
	// than as a dead server.
	if !strings.Contains(msg, "reads keep working") {
		t.Errorf("error %q does not say reads still work, so it reads as a dead server rather than a restart", msg)
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

// A fresh connection can report the SAME data_version the previous one did, so a
// check that cached on it would hand back a pre-migration answer and write into
// a store a newer Ghost owns. This drives exactly that: the pool's connection is
// discarded and replaced, the store is migrated underneath, and the write must
// still be refused.
//
// It is the regression test for the cache that was removed, and it uses only
// the public API — a cancelled query is enough to make modernc mark the
// connection unusable, and database/sql then opens a new one.
func TestWriteIsStillRefusedAfterTheConnectionIsReplaced(t *testing.T) {
	s, path := fileBackedStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "abc123", "/tmp/test", "test-project"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if _, err := s.Create(ctx, "abc123", Memory{
		Category: "fact", Content: "armed", Source: "mcp", Importance: 0.5,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	replacePoolConnection(t, s)

	// The store moves on while this server is not looking.
	stampFromAnotherHandle(t, path, SchemaVersion()+1)

	if _, err := s.Create(ctx, "abc123", Memory{
		Category: "fact", Content: "written after the migration", Source: "mcp", Importance: 0.5,
	}); !errors.Is(err, ErrStoreNewer) {
		t.Fatalf("write on a replacement connection = %v, want ErrStoreNewer — the probe must not answer from "+
			"a value cached against a connection that no longer exists", err)
	}
}

// replacePoolConnection makes database/sql discard its connection and open a
// fresh one, and FAILS the test if the pool did not actually do so.
//
// Two things about that are deliberate.
//
// The FIRST is that the assertion is the point. This test exists to prove the
// check does not answer from something cached against a dead connection, so if
// the pool quietly kept the connection the refusal below would be the ordinary
// one and the test would pass while proving nothing about the hard half of the
// story. A green test standing in for a mechanism it never exercised is worse
// than no test — and that is not hypothetical here: the first version of this
// helper cancelled a long-running query and slept, on the theory that modernc
// marks the interrupted connection unusable. It does not: with a full cartesian
// product the query finished, the pool never recycled, and the test passed green
// against a connection that was never replaced. The marker is what caught it.
//
// The SECOND is that the recycle is forced rather than provoked.
// SetMaxIdleConns(0) drops every idle connection synchronously, so the next
// query is guaranteed to dial a new one. Cancelling a query and hoping is not a
// way to write a test whose subject is "the old connection is gone".
//
// A TEMP table is what observes the connection, because its scope IS the
// connection: no other mechanism in the package can say which one a pooled query
// ran on, and with MaxOpenConns(1) the answer is unambiguous.
func replacePoolConnection(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, "CREATE TEMP TABLE IF NOT EXISTS ghost_conn_marker (id)"); err != nil {
		t.Fatalf("mark the current connection: %v", err)
	}
	s.db.SetMaxIdleConns(0)
	var survived int
	if err := s.db.QueryRowContext(ctx,
		"SELECT count(*) FROM sqlite_temp_master WHERE name = 'ghost_conn_marker'").Scan(&survived); err != nil {
		t.Fatalf("read the connection marker: %v", err)
	}
	if survived != 0 {
		t.Fatal("the pool kept its connection, so this test would prove nothing: the marker table created " +
			"before the replacement is still there, which means the same connection served the refused write")
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
