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

// firstContactStore holds the project read that opens the import until every
// client has reached it, so two simultaneous first contacts both see an empty
// project deterministically rather than by luck.
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
// dedup guard being one pool read before the loop with unconditional writes:
// two clients arriving together both read an empty project and each imported
// every file. Two handles on one database file stand in for two processes; the
// assertion is one row per source file however many clients import at once.
func TestConcurrentFirstContactImportsEachFileOnce(t *testing.T) {
	memDir := filepath.Join(t.TempDir(), "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTempFile(t, memDir, "project_architecture.md", "---\nname: Architecture overview\ndescription: System design notes\ntype: project\n---\n"+
		"The system uses a 3-layer architecture with memory, orchestrator, and AI layers.")

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

	var arrived, wg sync.WaitGroup
	arrived.Add(2)
	var (
		results [2]int
		errs    [2]error
	)
	for i := range results {
		db, err := memory.OpenDB(dbPath)
		if err != nil {
			t.Fatalf("OpenDB %d: %v", i, err)
		}
		defer func() { _ = db.Close() }()
		store := &firstContactStore{Store: memory.NewStore(db, logger), arrived: &arrived}
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], _, _, _, errs[i] = importFromDir(ctx, store, projectID, memDir, logger)
		}()
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
	if count != 1 || results[0]+results[1] != 1 {
		t.Errorf("project holds %d memories, importers reported %d and %d; want 1 row and 1 import in total", count, results[0], results[1])
	}
}
