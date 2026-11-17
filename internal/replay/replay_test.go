package replay

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func TestReplayRunsRecordedQueriesAgainstAPinnedSnapshot(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "snapshot.db")
	db, err := memory.OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	store := memory.NewStore(db, nil)
	if err := store.EnsureProject(ctx, "p", "/tmp/p", "p"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if _, _, _, err := store.Upsert(ctx, "p", "gotcha", "proxy timeout is set to 30 seconds", "mcp", 0.7, nil); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := store.RecordSearch(ctx, memory.SearchRecord{
		ProjectID: "p",
		Source:    "search",
		Query:     "proxy timeout",
		Results:   []string{"old-top", "old-second"},
	}); err != nil {
		t.Fatalf("RecordSearch: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close snapshot: %v", err)
	}

	queries, closeQueries, err := LoadQueries(path, 0)
	if err != nil {
		t.Fatalf("LoadQueries: %v", err)
	}
	defer closeQueries()
	if len(queries) != 1 || queries[0].Query != "proxy timeout" {
		t.Fatalf("loaded %+v, want the recorded query", queries)
	}

	current, closeCurrent, err := OpenSnapshot(path)
	if err != nil {
		t.Fatalf("OpenSnapshot: %v", err)
	}
	defer closeCurrent()

	results := Run(ctx, queries, RecordedRunner{}, StoreRunner{Store: current, Limit: 5}, 5)
	if len(results) != 1 {
		t.Fatalf("Run returned %d results, want 1", len(results))
	}
	if results[0].Err != nil {
		t.Fatalf("replay failed: %v", results[0].Err)
	}
	if len(results[0].Diff.CurrentTop) == 0 {
		t.Error("the current build ranked no memory for a query the snapshot can answer")
	}
	if !results[0].Diff.TopHitChanged {
		t.Error("the recorded top hit and the current top hit differ, so the gate must say so")
	}
}
