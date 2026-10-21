package memory

// #1738: the diagnostic read does not wait for a write in flight.
//
// A write holds the store's mutex for its whole transaction, and the primary
// handle is pinned to one connection, so a read that goes through the primary
// handle waits for the write to commit. The diagnostic read goes through the
// read-only handle instead, where a WAL reader sees the last committed state
// and never waits for a writer.

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

// diagnosticTestStore is a file-backed store with the read handle the
// production server is built with. An in-memory database cannot have one:
// ":memory:" is per-connection, so a second handle would open a second,
// empty database.
func diagnosticTestStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ghost.db")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	readDB, err := OpenReadDB(path)
	if err != nil {
		t.Fatalf("OpenReadDB: %v", err)
	}
	t.Cleanup(func() { _ = readDB.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := NewStoreWithRead(db, readDB, logger)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "p", "/p", "p"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	id, _, _, err := store.Upsert(ctx, "p", "fact", "the relay listens on port 2222", "mcp", 0.5, nil)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	// A vector recorded under a RETIRED identity, so the coverage counts the
	// diagnostic view reports say whether the view carried the store's own
	// vector identity across: a view that lost it would count this vector as
	// covered.
	store.SetEmbeddingIdentity("current-space:v1")
	if err := store.StoreEmbedding(ctx, id, []float32{1, 2, 3}, "retired-space:v0"); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}
	return store, db
}

// TestTheDiagnosticReadAnswersWhileAWriteIsInFlight: the store's own read
// waits for the write, and the diagnostic read does not. Both halves are
// observed here: the first is what the diagnostic read exists to avoid, and a
// test that only saw the second could not tell the difference between a
// diagnostic read and a store whose writes are fast.
func TestTheDiagnosticReadAnswersWhileAWriteIsInFlight(t *testing.T) {
	ctx := context.Background()
	store, db := diagnosticTestStore(t)

	// The writer: it holds the store's mutex and the store's one connection,
	// which is what a project delete or a reflection apply does for the whole
	// of its transaction.
	store.mu.Lock()
	defer store.mu.Unlock()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin the write transaction: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// The store's own read waits for it.
	waited := make(chan error, 1)
	go func() {
		_, err := store.ListProjects(ctx)
		waited <- err
	}()
	select {
	case err := <-waited:
		t.Fatalf("the store's own read answered while a write held the lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	// The diagnostic read answers, and its numbers are the store's. Its own
	// deadline is generous and bounded: a read that waited for the write would
	// otherwise hang until the test binary's timeout, which says less about
	// what broke than a failure that names the wait.
	readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	read, err := store.BeginDiagnosticRead(readCtx)
	if err != nil {
		t.Fatalf("the diagnostic read did not answer while a write was in flight: %v", err)
	}
	if read == nil {
		t.Fatal("a store with a read handle has no diagnostic read")
	}
	defer read.Close() //nolint:errcheck

	projects, err := read.Store().ListProjects(readCtx)
	if err != nil {
		t.Fatalf("the diagnostic read did not answer while a write was in flight: %v", err)
	}
	if len(projects) != 1 || projects[0].ID != "p" {
		t.Fatalf("the diagnostic read reported %d project(s) %+v, want the one project", len(projects), projects)
	}
	count, err := read.Store().CountMemories(readCtx, "p")
	if err != nil {
		t.Fatalf("the diagnostic count did not answer: %v", err)
	}
	if count != 1 {
		t.Errorf("the diagnostic count is %d, want 1", count)
	}
	// The coverage counts, and the vector identity they were taken against: a
	// view that lost the store's identity would count the retired vector as
	// covered.
	embedded, stale, total, err := read.Store().EmbeddingStats(readCtx)
	if err != nil {
		t.Fatalf("the diagnostic coverage did not answer: %v", err)
	}
	if embedded != 0 || stale != 1 || total != 1 {
		t.Errorf("the diagnostic coverage is (embedded %d, stale %d, total %d), want (0, 1, 1) \u2014 the view did not carry the store's vector identity",
			embedded, stale, total)
	}
	storeVersion, buildVersion, newer, err := read.Store().StoreVersionStatus(readCtx)
	if err != nil {
		t.Fatalf("the diagnostic store version did not answer: %v", err)
	}
	if storeVersion != SchemaVersion() || buildVersion != SchemaVersion() || newer {
		t.Errorf("the diagnostic store version is (store %d, build %d, newer %t), want both at %d and not newer",
			storeVersion, buildVersion, newer, SchemaVersion())
	}
}

// TestAStoreWithNoReadHandleHasNoDiagnosticRead: an in-memory store, a bench
// store and a store built without NewStoreWithRead have no second handle to
// read through. The diagnostic read says so rather than pretending, and the
// caller falls back to the store's own reads.
func TestAStoreWithNoReadHandleHasNoDiagnosticRead(t *testing.T) {
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))

	read, err := store.BeginDiagnosticRead(context.Background())
	if err != nil {
		t.Fatalf("BeginDiagnosticRead: %v", err)
	}
	if read != nil {
		t.Fatal("a store with no read handle offered a diagnostic read")
	}
	if read.Store() != nil {
		t.Error("a diagnostic read that does not exist returned a store")
	}
	if err := read.Close(); err != nil {
		t.Errorf("closing a diagnostic read that does not exist: %v", err)
	}
}
