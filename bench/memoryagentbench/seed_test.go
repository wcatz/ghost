package main

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func TestSeedFactsOrdersTimestamps(t *testing.T) {
	ctx := context.Background()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	store := memory.NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer store.Close() //nolint:errcheck // store.Close() closes the underlying db too

	const project = "test-mabench-seed"
	if err := store.EnsureProject(ctx, project, "/bench/"+project, project); err != nil {
		t.Fatal(err)
	}

	facts := []string{"A is founded by X.", "A is founded by Y.", "A is founded by Z."}
	ids, err := seedFacts(ctx, store, db, project, facts)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != len(facts) {
		t.Fatalf("got %d ids, want %d", len(ids), len(facts))
	}

	mems, err := store.GetByIDs(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]memory.Memory, len(mems))
	for _, m := range mems {
		byID[m.ID] = m
	}
	for i := 1; i < len(ids); i++ {
		prev, cur := byID[ids[i-1]], byID[ids[i]]
		if prev.UpdatedAt >= cur.UpdatedAt {
			t.Errorf("fact %d (%q, updated_at=%s) is not strictly older than fact %d (%q, updated_at=%s)",
				i-1, prev.Content, prev.UpdatedAt, i, cur.Content, cur.UpdatedAt)
		}
	}
}
