package memory

import (
	"context"
	"testing"
)

// pinnedState reports whether the memory with this id is pinned.
func pinnedState(t *testing.T, s *Store, id string) bool {
	t.Helper()
	mems, err := s.GetByIDs(context.Background(), []string{id})
	if err != nil || len(mems) != 1 {
		t.Fatalf("GetByIDs(%s): %v (n=%d)", id, err, len(mems))
	}
	return mems[0].Pinned
}

// TestUpsertPin_MarksTheNewRowPinned: a save may ask for the memory it writes to
// be exempt from consolidation in the same call (#549), and pinning is opt-in.
func TestUpsertPin_MarksTheNewRowPinned(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name    string
		content string
		opts    UpsertOptions
		want    bool
	}{
		{"pin requested", "the store pins MaxOpenConns(1) because a pool query inside a write tx deadlocks", UpsertOptions{Pin: true}, true},
		{"default is unpinned", "the laptop is not a build host", UpsertOptions{}, false},
	} {
		id, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", tc.content, "mcp", 0.8, nil, tc.opts)
		if err != nil {
			t.Fatalf("%s: UpsertWithOptions: %v", tc.name, err)
		}
		if got := pinnedState(t, s, id); got != tc.want {
			t.Errorf("%s: pinned = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestUpsertPin_FoldPinsTheExistingRowToo: a near-duplicate save that folds keeps
// the older row as the surviving text, so pinning only the new row would leave
// the fact the caller asked to protect consolidatable. Both are pinned.
func TestUpsertPin_FoldPinsTheExistingRowToo(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	existing, _, _, err := s.Upsert(ctx, testProject, "gotcha",
		"the vector identity is compared as an opaque string, never parsed", "mcp", 0.6, nil)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	folded, dup, _, err := s.UpsertWithOptions(ctx, testProject, "gotcha",
		"the vector identity is an opaque compared string, not parsed", "mcp", 0.6, nil, UpsertOptions{Pin: true})
	if err != nil {
		t.Fatalf("UpsertWithOptions: %v", err)
	}
	if dup == "" {
		t.Skip("wording did not fold; the pin-on-existing-row branch was not reached")
	}
	for _, id := range []string{existing, folded} {
		if !pinnedState(t, s, id) {
			t.Errorf("row %s: Pin: true left a row consolidation may rewrite", id)
		}
	}
}

// TestUpsertPin_FoldOnlyFoldPinsTheTargetWithoutStoringTheWording: FoldOnly stores
// no new row, so the pin request must land on the target.
func TestUpsertPin_FoldOnlyFoldPinsTheTargetWithoutStoringTheWording(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	target, _, _, err := s.Upsert(ctx, testProject, "fact", "cherry is used for the release commit", "reflection", 0.6, nil)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	id, dup, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
		"Cherry is used for the release commit", "reflection", 0.6, nil, UpsertOptions{FoldOnly: true, Pin: true})
	if err != nil {
		t.Fatalf("UpsertWithOptions: %v", err)
	}
	if dup != target {
		t.Skip("wording did not fold; the FoldOnly pin branch was not reached")
	}
	if id != target {
		t.Fatalf("FoldOnly returned a new id %s; expected the folded target %s", id, target)
	}
	if !pinnedState(t, s, target) {
		t.Error("a FoldOnly pin left the target unpinned")
	}
}
