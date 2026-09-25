package memory

import (
	"fmt"
	"testing"
)

// TestSearchHybridKeywordOnlyBackfillsDeletedRow is the FTS-only half of the
// hydration contract.
//
// The keyword leg returns fully hydrated rows, so it is tempting to select the
// window straight out of that slice and skip the read by id. That is wrong: the
// leg's rows are the snapshot an earlier statement returned, and a concurrent
// writer can delete a selected row before the search returns. Reading by id is
// what makes the vanished ID disappear so the next candidate can backfill;
// taking the leg's copy would emit a row that no longer exists, with the stale
// category, pinned and created_at that decay then ranks on.
//
// The hybrid path gets this for free because its pool is vector-only. The
// keyword-only path is where the shortcut looks attractive, so it is the one
// that needs the test.
func TestSearchHybridKeywordOnlyBackfillsDeletedRow(t *testing.T) {
	store, ctx := setupTestStore(t)

	// Enough candidates that losing the top one still leaves a full window.
	ids := make([]string, 0, 6)
	for i := 0; i < 6; i++ {
		id := createTestMemory(t, store, ctx, fmt.Sprintf("database configuration candidate %d", i))
		ids = append(ids, id)
	}

	beforeHybridHydrateFn.Store(func(selected []string) {
		if len(selected) == 0 {
			return
		}
		if err := store.Delete(ctx, selected[0]); err != nil {
			t.Fatalf("delete selected candidate: %v", err)
		}
	})
	t.Cleanup(func() { beforeHybridHydrateFn.Store(func([]string) {}) })

	got, err := store.SearchHybrid(ctx, "test-proj", "database configuration", nil, 5)
	if err != nil {
		t.Fatalf("SearchHybrid: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d results after the top-ranked row was deleted, want 5: the keyword-only "+
			"path must re-read by id so the vanished row can be backfilled", len(got))
	}
	for _, m := range got {
		if m.ID == ids[0] {
			t.Fatalf("result set still contains the deleted row %s", m.ID)
		}
	}
	// The replacement must be a real row, not a zero-valued Memory: a backfill
	// that only padded the slice would pass the length check above.
	for _, m := range got {
		if m.Content == "" || m.Category == "" {
			t.Fatalf("backfilled row %+v is not hydrated; content and category must come from the "+
				"store, not from a missing row", m)
		}
	}

	// And the store must agree the row is gone, so the assertion above is about
	// this search and not about the delete silently failing.
	remaining, err := store.GetByIDs(ctx, ids[:1])
	if err != nil {
		t.Fatalf("GetByIDs(deleted): %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("deleted row %s is still in the store; the fixture is not testing the race", ids[0])
	}
}

// TestSearchHybridReadsOnlyTheWindowWhenNothingVanished pins the cost side of the
// same contract. When no window ID is missing — the overwhelmingly common case —
// the search must not fall back to reading the whole candidate pool, because that
// pool is up to limit*2 rows and exists only to feed the backfill.
func TestSearchHybridReadsOnlyTheWindowWhenNothingVanished(t *testing.T) {
	store, ctx, reads := hydrationCountingStore(t)
	for i := 0; i < 8; i++ {
		createTestMemory(t, store, ctx, fmt.Sprintf("database configuration candidate %d", i))
	}

	reads.reset()
	if _, err := store.SearchHybrid(ctx, "test-proj", "database configuration", nil, 5); err != nil {
		t.Fatalf("SearchHybrid: %v", err)
	}

	sizes := reads.idListSizes()
	if len(sizes) != 1 {
		t.Fatalf("issued %d id-list reads %v, want exactly 1: nothing vanished, so the wider "+
			"candidate pool must not be read", len(sizes), sizes)
	}
	if sizes[0] > 5 {
		t.Errorf("read %d ids, want at most the 5-row window: the candidate pool is up to "+
			"limit*2 rows and exists only to feed the backfill", sizes[0])
	}
}

// TestSearchHybridReadsThePoolWhenAWindowRowVanished is the other half: the wider
// read is not dead code, it is the backfill source, and it must still happen when
// a selected row is deleted between the leg query and hydration.
func TestSearchHybridReadsThePoolWhenAWindowRowVanished(t *testing.T) {
	store, ctx, reads := hydrationCountingStore(t)
	ids := make([]string, 0, 6)
	for i := 0; i < 6; i++ {
		ids = append(ids, createTestMemory(t, store, ctx, fmt.Sprintf("database configuration candidate %d", i)))
	}

	beforeHybridHydrateFn.Store(func(selected []string) {
		if len(selected) == 0 {
			return
		}
		if err := store.Delete(ctx, selected[0]); err != nil {
			t.Fatalf("delete selected candidate: %v", err)
		}
	})
	t.Cleanup(func() { beforeHybridHydrateFn.Store(func([]string) {}) })

	reads.reset()
	got, err := store.SearchHybrid(ctx, "test-proj", "database configuration", nil, 5)
	if err != nil {
		t.Fatalf("SearchHybrid: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d results after the top-ranked row was deleted, want 5: the keyword-only "+
			"path must re-read by id so the vanished row can be backfilled", len(got))
	}
	for _, m := range got {
		if m.ID == ids[0] {
			t.Fatalf("result set still contains the deleted row %s", m.ID)
		}
		// A backfill that only padded the slice would pass the length check, so
		// require the replacement to be a real hydrated row.
		if m.Content == "" || m.Category == "" {
			t.Fatalf("backfilled row %+v is not hydrated; content and category must come from "+
				"the store, not from a row that no longer exists", m)
		}
	}

	sizes := reads.idListSizes()
	if len(sizes) < 2 {
		t.Fatalf("issued %d id-list reads %v, want a second read of the candidate pool: that is "+
			"where the backfill replacement comes from", len(sizes), sizes)
	}
	if sizes[len(sizes)-1] <= sizes[0] {
		t.Errorf("backfill read %v is not wider than the window read %v; the pool must be the "+
			"larger of the two", sizes, sizes[0])
	}
}
