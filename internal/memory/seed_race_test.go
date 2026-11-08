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

// TestSeedGlobalMemoriesIsOneTransactionAcrossHandles is the concurrency
// statement of the rule that a read deciding a write belongs inside the write
// transaction (docs/architecture.md, Boundaries).
//
// SeedGlobalMemories is called on every open — by bootstrap, so by every
// editor-launched MCP server and by any CLI command that races one. Two
// processes therefore meet on one file. s.mu is a per-Store mutex and closes
// nothing across processes, so what has to serialize them is SQLite's write
// lock, and the only way to make the existence probe see a row another handle
// has committed is to take that lock FIRST and probe inside it.
//
// The probe used to run on the pool as an autocommit read, so it answered
// about a moment before the insert's transaction existed. Two handles then
// both read zero and both inserted: a byte-identical pinned
// source = 'builtin' copy that nothing repairs, in the Global section of every
// session-start block and every project context, forever.
func TestSeedGlobalMemoriesIsOneTransactionAcrossHandles(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "seed-race.sqlite")

	first, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB first: %v", err)
	}
	defer func() { _ = first.Close() }()
	second, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB second: %v", err)
	}
	defer func() { _ = second.Close() }()

	quiet := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a, b := NewStore(first, quiet), NewStore(second, quiet)

	// The probe is a table scan filtered on content, so its cost is the number
	// of _global rows it has to read. Padding it is what gives two handles a
	// window they can both be inside: without it a COUNT over a handful of rows
	// finishes in microseconds and the race it exists to catch turns on
	// scheduler luck rather than on the ordering of the statements.
	if err := a.EnsureProject(ctx, GlobalProjectID, GlobalProjectID, "global"); err != nil {
		t.Fatalf("ensure _global: %v", err)
	}
	padGlobalScan(ctx, t, a)
	const seedRows = `SELECT COUNT(*) FROM memories WHERE project_id = '_global' AND source = 'builtin'`
	for round := 0; round < seedRaceRounds; round++ {
		// A fresh store every round: the seed is written by content, so the
		// only way to give the race another chance is to clear its own work and
		// let both handles start from a store with no builtin row.
		if _, err := a.db.ExecContext(ctx, `DELETE FROM memories WHERE source = 'builtin'`); err != nil {
			t.Fatalf("round %d: clear builtin rows: %v", round, err)
		}

		var wg sync.WaitGroup
		start := make(chan struct{})
		for _, store := range []*Store{a, b} {
			wg.Add(1)
			go func(store *Store) {
				defer wg.Done()
				<-start
				if err := store.SeedGlobalMemories(ctx); err != nil {
					t.Errorf("round %d: SeedGlobalMemories: %v", round, err)
				}
			}(store)
		}
		close(start)
		wg.Wait()

		var rows int
		if err := a.db.QueryRowContext(ctx, seedRows).Scan(&rows); err != nil {
			t.Fatalf("round %d: count builtin rows: %v", round, err)
		}
		if rows != 1 {
			t.Fatalf("round %d: %d builtin _global rows, want 1 — a read that decides a write ran outside the write transaction", round, rows)
		}
	}
}

const (
	// seedRaceRounds is the number of concurrent seeding attempts the test
	// makes. The ordering under test is not guaranteed by any sleep, so it is
	// repeated rather than assumed.
	seedRaceRounds = 20
	// seedRacePadRows/seedRacePadBytes size the scan widening below. Enough
	// rows that the probe costs milliseconds, and a small database.
	seedRacePadRows  = 400
	seedRacePadBytes = 8 << 10
)

// padGlobalScan fills _global with rows large enough that the seed probe's
// COUNT has real work to do. They are ordinary memories in another category, so
// they cannot be mistaken for a seed by anything that counts builtin rows.
func padGlobalScan(ctx context.Context, t *testing.T, s *Store) {
	t.Helper()
	filler := strings.Repeat("x", seedRacePadBytes)
	// One transaction of plain inserts: the rows only have to exist, and a
	// Create per row ran the whole save path 400 times, which is what made this
	// test the second slowest under the race detector.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin pad: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	for i := 0; i < seedRacePadRows; i++ {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO memories (id, project_id, category, content, source) VALUES (?, ?, 'fact', ?, 'mcp')`,
			fmt.Sprintf("%032x", i+1), GlobalProjectID, filler,
		); err != nil {
			t.Fatalf("pad global row %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit pad: %v", err)
	}
}
