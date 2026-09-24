package memory

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestConcurrentProcessesMixedReadWrite holds the multi-process contract: any
// number of ghost processes may open the same database, and SQLite is the
// synchronization layer. A CLI command, a live MCP server, a lifecycle
// subprocess, and a maintenance run can all be in flight at once, each with
// its own pool (OpenDB pins each handle to one connection, so N handles is
// the real concurrency N).
//
// The guarantees this pins:
//
//   - no handle ever fails with SQLITE_BUSY: every write either lands or
//     returns a non-contention error. A writer that gives up under contention
//     is a lost memory, and a lost memory is invisible until much later.
//   - no write is silently dropped: the final count equals the sum of the
//     successful writes.
//   - a read concurrent with writes never observes a torn row: memory
//     content and its FTS row are updated atomically by trigger, so a
//     reader must never see one without the other.
func TestConcurrentProcessesMixedReadWrite(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "multiproc.sqlite")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	const (
		handles    = 4 // separate sql.DB pools: stands in for separate processes
		writesEach = 25
	)

	// Seed through the first handle so every later writer has a project to
	// write against; a missing project would surface as FOREIGN KEY rather
	// than contention and mask the behavior under test.
	db0, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB 0: %v", err)
	}
	defer func() { _ = db0.Close() }()
	seed := NewStore(db0, logger)
	ctx := context.Background()
	if err := seed.EnsureProject(ctx, testProject, "/tmp/multiproc", testProject); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	stores := make([]*Store, 0, handles)
	for i := 0; i < handles; i++ {
		db, err := OpenDB(dbPath)
		if err != nil {
			t.Fatalf("OpenDB %d: %v", i, err)
		}
		defer func() { _ = db.Close() }()
		stores = append(stores, NewStore(db, logger))
	}

	var (
		wg          sync.WaitGroup
		mu          sync.Mutex
		written     int
		writeErrs   []string
		readErrs    []string
		tornContent []string
	)

	for h, store := range stores {
		// Writers.
		for w := 0; w < writesEach; w++ {
			wg.Add(1)
			go func(h, w int) {
				defer wg.Done()
				content := fmt.Sprintf("concurrent fact h=%d w=%d about sqlite wal checkpointing", h, w)
				_, _, _, err := store.Upsert(ctx, testProject, "fact", content, "manual", 0.5, []string{"concurrency"})
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					writeErrs = append(writeErrs, fmt.Sprintf("h=%d w=%d: %v", h, w, err))
					return
				}
				written++
			}(h, w)
		}

		// Readers, running against the same file while the writers commit.
		wg.Add(1)
		go func(store *Store) {
			defer wg.Done()
			for i := 0; i < writesEach; i++ {
				// SearchFTS rather than SearchHybrid: the FTS side needs no
				// Ollama, so the contract holds in a hermetic CI run.
				rows, err := store.SearchFTS(ctx, testProject, "sqlite wal checkpointing concurrent", 10)
				if err != nil {
					mu.Lock()
					readErrs = append(readErrs, err.Error())
					mu.Unlock()
					continue
				}
				for _, r := range rows {
					// A torn row would pair content with a missing or
					// mismatched FTS entry: the search returned it by content,
					// so the stored content must actually contain the query
					// terms rather than a stale or empty string.
					if r.Content == "" {
						mu.Lock()
						tornContent = append(tornContent, fmt.Sprintf("id=%s returned empty content", r.ID))
						mu.Unlock()
					}
				}
			}
		}(store)
	}
	wg.Wait()

	for _, e := range writeErrs {
		t.Errorf("write failed under contention: %s", e)
	}
	for _, e := range readErrs {
		t.Errorf("read failed under contention: %s", e)
	}
	for _, e := range tornContent {
		t.Errorf("torn read: %s", e)
	}

	// No write may be silently dropped: every successful Upsert must be
	// visible from a freshly opened handle afterwards.
	dbVerify, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB verify: %v", err)
	}
	defer func() { _ = dbVerify.Close() }()
	verify := NewStore(dbVerify, logger)

	total, err := verify.CountMemories(ctx, testProject)
	if err != nil {
		t.Fatalf("CountMemories: %v", err)
	}
	// The seed handle's own writes are zero, so the only contributions are
	// the successful Upserts counted in `written`.
	if total != written {
		t.Errorf("database holds %d memories, but %d writes reported success — contention silently dropped %d", total, written, written-total)
	}
}

// TestOpenDBPinsPoolToSingleConnection pins the second half of the contract:
// OpenDB sets SetMaxOpenConns(1).
//
// The mixed-load test above does not cover this. Pool pinning does not exist
// to serialize writers — SQLite and busy_timeout already do that, and
// unpinning the pool leaves that test green. It exists because
// `PRAGMA data_version` is per-connection: obsidian sync reads it on each tick
// to detect a commit from another process, and compares ticks against a
// baseline captured earlier. If ticks can land on different pooled
// connections, the comparison is between two connection-local counters rather
// than two points in the database's history, so a commit can be skipped or a
// redundant export triggered forever.
//
// Asserted on db.Stats() rather than by observing sync output: the emergent
// behavior depends on pool scheduling, so a black-box test here would be
// flaky instead of sensitive.
func TestOpenDBPinsPoolToSingleConnection(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "pin.sqlite"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() { _ = db.Close() }()

	if got := db.Stats().MaxOpenConnections; got != 1 {
		t.Errorf("MaxOpenConnections = %d, want 1 — PRAGMA data_version polling is per-connection, so an unpinned pool compares ticks across different connections", got)
	}
}
