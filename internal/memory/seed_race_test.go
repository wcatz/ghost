package memory

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestSeedGlobalMemoriesIsOneTransactionAcrossHandles(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "seed-race.sqlite")
	first, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	second, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()

	quiet := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a, b := NewStore(first, quiet), NewStore(second, quiet)
	if err := a.EnsureProject(ctx, GlobalProjectID, GlobalProjectID, "global"); err != nil {
		t.Fatal(err)
	}
	padGlobalScan(ctx, t, a)
	const seedRows = `SELECT COUNT(*) FROM memories WHERE project_id = '_global' AND source = 'builtin'`
	for round := 0; round < seedRaceRounds; round++ {
		if _, err := a.db.ExecContext(ctx, `DELETE FROM memories WHERE source = 'builtin'`); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		for _, store := range []*Store{a, b} {
			wg.Add(1)
			go func(store *Store) {
				defer wg.Done()
				<-start
				if err := store.SeedGlobalMemories(ctx); err != nil {
					t.Errorf("round %d: %v", round, err)
				}
			}(store)
		}
		close(start)
		wg.Wait()
		var rows int
		if err := a.db.QueryRowContext(ctx, seedRows).Scan(&rows); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if rows != 1 {
			t.Fatalf("round %d: got %d builtin rows", round, rows)
		}
	}
}

const (
	seedRaceRounds   = 20
	seedRacePadRows  = 400
	seedRacePadBytes = 8 << 10
)

func padGlobalScan(ctx context.Context, t *testing.T, s *Store) {
	t.Helper()
	filler := strings.Repeat("x", seedRacePadBytes)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for i := 0; i < seedRacePadRows; i++ {
		if _, err := tx.ExecContext(ctx, `INSERT INTO memories (id, project_id, category, content, source) VALUES (?, ?, 'fact', ?, 'mcp')`, fmt.Sprintf("%032x", i+1), GlobalProjectID, filler); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
