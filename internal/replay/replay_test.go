package replay

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func replayStrPtr(s string) *string { return &s }

func TestReplayRanksSearchAllRowsAsWholeCorpusReads(t *testing.T) {
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
	id, _, _, err := store.Upsert(ctx, "p", "gotcha", "proxy timeout is set to 30 seconds", "mcp", 0.7, nil)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	recorded, err := store.SearchHybridAll(ctx, "proxy timeout", nil, 10)
	if err != nil {
		t.Fatalf("SearchHybridAll: %v", err)
	}
	if len(recorded) != 1 {
		t.Fatalf("recorded ranking holds %d rows, want 1", len(recorded))
	}
	if err := store.RecordSearch(ctx, memory.SearchRecord{
		ProjectID: "_global",
		Source:    memory.SearchAllSource,
		Query:     "proxy timeout",
		Results:   []string{recorded[0].ID},
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
	current, closeCurrent, err := OpenSnapshot(path)
	if err != nil {
		t.Fatalf("OpenSnapshot: %v", err)
	}
	defer closeCurrent()

	results := Run(ctx, queries, RecordedRunner{}, StoreRunner{Store: current, Limit: 10}, 5)
	if len(results) != 1 {
		t.Fatalf("Run returned %d results, want 1", len(results))
	}
	if results[0].Err != nil {
		t.Fatalf("replay failed: %v", results[0].Err)
	}
	if results[0].Diff.CurrentTop != id {
		t.Errorf("current top = %q, want the memory in project %q (id %s); a search_all row scoped to _global would rank nothing",
			results[0].Diff.CurrentTop, "p", id)
	}
	if results[0].Diff.TopHitChanged {
		t.Error("a search_all row replayed with its own ranking reported a false changed top hit")
	}
}

func TestReplayRanksACallAtItsOwnRecordedInstant(t *testing.T) {
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
	// The call was made in January, before the memory's validity window opened
	// in March. A constant clock that lands after March would rank a memory the
	// caller could never have seen and report a regression that never happened.
	validFrom := "2026-03-01 00:00:00"
	if _, _, _, err := store.UpsertWithOptions(ctx, "p", "gotcha",
		"proxy timeout is set to 30 seconds", "mcp", 0.7, nil,
		memory.UpsertOptions{Validity: memory.Validity{ValidFrom: &validFrom}}); err != nil {
		t.Fatalf("UpsertWithOptions: %v", err)
	}
	if err := store.RecordSearch(ctx, memory.SearchRecord{
		ProjectID: "p", Source: "search", Query: "proxy timeout", Results: []string{}, Limit: 5,
	}); err != nil {
		t.Fatalf("RecordSearch: %v", err)
	}
	if _, err := db.Exec(`UPDATE audit_log SET timestamp = ? WHERE action = 'search'`,
		"2026-01-01 12:00:00"); err != nil {
		t.Fatalf("stamp the recorded call: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close snapshot: %v", err)
	}

	queries, closeQueries, err := LoadQueries(path, 0)
	if err != nil {
		t.Fatalf("LoadQueries: %v", err)
	}
	defer closeQueries()
	current, closeCurrent, err := OpenSnapshot(path)
	if err != nil {
		t.Fatalf("OpenSnapshot: %v", err)
	}
	defer closeCurrent()

	results := Run(ctx, queries, RecordedRunner{}, StoreRunner{Store: current, Limit: 5}, 5)
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("replay: %+v", results)
	}
	if results[0].Diff.CurrentTop != "" {
		t.Errorf("current top = %q, want no row: this memory was not yet valid at the call's recorded instant",
			results[0].Diff.CurrentTop)
	}
}

func TestReplayHonoursTheRecordedAsOfBinding(t *testing.T) {
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
	id, _, _, err := store.Upsert(ctx, "p", "fact", "zephyr cache invalidation is tricky", "mcp", 0.7, nil)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	// Put the version the caller saw at a known instant, then edit the row so
	// only a replay that honours as_of can still match the recorded query.
	if _, err := db.Exec(`UPDATE memory_history SET recorded_at = ? WHERE memory_id = ?`,
		"2026-03-01 00:00:00", id); err != nil {
		t.Fatalf("stamp the recorded version: %v", err)
	}
	const asOf = "2026-03-01T12:00:00Z"
	if err := store.RecordSearch(ctx, memory.SearchRecord{
		ProjectID: "p", Source: "search", Query: "zephyr", Results: []string{id}, AsOf: asOf, Limit: 5,
	}); err != nil {
		t.Fatalf("RecordSearch: %v", err)
	}
	if err := store.UpdateMemory(ctx, "p", id, replayStrPtr("quasar batch scheduler"), nil, nil, nil); err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close snapshot: %v", err)
	}

	queries, closeQueries, err := LoadQueries(path, 0)
	if err != nil {
		t.Fatalf("LoadQueries: %v", err)
	}
	defer closeQueries()
	current, closeCurrent, err := OpenSnapshot(path)
	if err != nil {
		t.Fatalf("OpenSnapshot: %v", err)
	}
	defer closeCurrent()

	results := Run(ctx, queries, RecordedRunner{}, StoreRunner{Store: current, Limit: 5}, 5)
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("replay: %+v", results)
	}
	if results[0].Diff.CurrentTop != id {
		t.Errorf("current top = %q, want %q: a historical call must rank the version at its recorded as_of, not the edited row",
			results[0].Diff.CurrentTop, id)
	}
}

func TestReplayComparesACallAtItsOwnResultLimit(t *testing.T) {
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
	for _, content := range []string{"proxy timeout is set to 30 seconds", "proxy retry budget is three"} {
		if _, _, _, err := store.Upsert(ctx, "p", "fact", content, "mcp", 0.7, nil); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}
	top, err := store.SearchHybrid(ctx, "p", "proxy", nil, 1)
	if err != nil {
		t.Fatalf("SearchHybrid: %v", err)
	}
	if len(top) != 1 {
		t.Fatalf("baseline ranking holds %d rows, want 1", len(top))
	}
	if err := store.RecordSearch(ctx, memory.SearchRecord{
		ProjectID: "p", Source: "search", Query: "proxy", Results: []string{top[0].ID}, Limit: 1,
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
	current, closeCurrent, err := OpenSnapshot(path)
	if err != nil {
		t.Fatalf("OpenSnapshot: %v", err)
	}
	defer closeCurrent()

	results := Run(ctx, queries, RecordedRunner{}, StoreRunner{Store: current, Limit: 5}, 5)
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("replay: %+v", results)
	}
	d := results[0].Diff
	if d.TopK != 1 {
		t.Errorf("comparison window = %d, want 1 (the call's own limit)", d.TopK)
	}
	if len(d.Added) != 0 || len(d.Removed) != 0 || len(d.Shifts) != 0 || d.TopHitChanged {
		t.Errorf("a limit-1 call reported false ranking changes: %+v", d)
	}
}

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
