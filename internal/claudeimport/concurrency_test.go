package claudeimport

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// firstContactStore is one client's handle on a shared database, with the
// project read that opens the import held until every client has reached it.
//
// That read decides whether the import writes, so holding it is how two
// simultaneous first contacts are made deterministic: without the hold the two
// goroutines below usually serialize by luck, and a test that only sometimes
// reproduces the interleaving is not a regression test. With it, both clients
// read the project as empty, which is exactly the state two real clients reach
// when they arrive together.
type firstContactStore struct {
	*memory.Store
	arrived *sync.WaitGroup
}

func (s *firstContactStore) GetAll(ctx context.Context, projectID string, limit int) ([]memory.Memory, error) {
	s.arrived.Done()
	s.arrived.Wait()
	return s.Store.GetAll(ctx, projectID, limit)
}

// TestConcurrentFirstContactImportsEachFileOnce is the regression test for the
// doubling this package's dedup guard had: the guard was one pool read of the
// project, taken before the loop, and the writes were unconditional — so two
// clients that reached first contact together both read an empty project and
// both imported every file, landing each one twice as byte-identical rows.
//
// Two handles on one database file stand in for two processes, and every call
// that must be atomic has to hold across them. The assertion is the one the
// issue states: one row per source file, however many clients import it at
// once.
func TestConcurrentFirstContactImportsEachFileOnce(t *testing.T) {
	memDir := filepath.Join(t.TempDir(), "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTempFile(t, memDir, "project_architecture.md", `---
name: Architecture overview
description: System design notes
type: project
---
The system uses a 3-layer architecture with memory, orchestrator, and AI layers.`)

	ctx := context.Background()
	const projectID = "test-project"
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	dbPath := filepath.Join(t.TempDir(), "first-contact.sqlite")
	seedDB, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB seed: %v", err)
	}
	defer func() { _ = seedDB.Close() }()
	seed := memory.NewStore(seedDB, logger)
	if err := seed.EnsureProject(ctx, projectID, filepath.Dir(memDir), projectID); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	var arrived sync.WaitGroup
	arrived.Add(2)

	stores := make([]*firstContactStore, 2)
	for i := range stores {
		db, err := memory.OpenDB(dbPath)
		if err != nil {
			t.Fatalf("OpenDB %d: %v", i, err)
		}
		defer func() { _ = db.Close() }()
		stores[i] = &firstContactStore{Store: memory.NewStore(db, logger), arrived: &arrived}
	}

	var (
		wg      sync.WaitGroup
		results [2]int
		errs    [2]error
	)
	for i, store := range stores {
		wg.Add(1)
		go func(i int, store *firstContactStore) {
			defer wg.Done()
			results[i], _, errs[i] = importFromDir(ctx, store, projectID, memDir, logger)
		}(i, store)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("importFromDir %d: %v", i, err)
		}
	}

	count, err := seed.CountMemories(ctx, projectID)
	if err != nil {
		t.Fatalf("CountMemories: %v", err)
	}
	if count != 1 {
		t.Errorf("project holds %d memories after two concurrent first contacts, want 1 (one per source file); importers reported %d and %d", count, results[0], results[1])
	}
	if got := results[0] + results[1]; got != 1 {
		t.Errorf("importers together reported %d new memories, want 1 — the file is imported once, by whichever client won", got)
	}
}
