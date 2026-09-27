package memory

import (
	"context"
	"testing"
)

// TestUpsertPin_MarksTheNewRowPinned: a save may ask for the memory it is
// writing to be exempt from consolidation, in the same call (#549). Before this
// the only way to opt a memory out was a second ghost_memory_pin call, so the
// window between the two — and any turn that ended before the second call —
// left the memory consolidatable.
func TestUpsertPin_MarksTheNewRowPinned(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, _, _, err := s.UpsertWithOptions(ctx, testProject, "architecture",
		"the store pins MaxOpenConns(1) because a pool query inside a write tx deadlocks",
		"mcp", 0.8, nil, UpsertOptions{Pin: true})
	if err != nil {
		t.Fatalf("UpsertWithOptions: %v", err)
	}

	mems, err := s.GetByIDs(ctx, []string{id})
	if err != nil || len(mems) != 1 {
		t.Fatalf("GetByIDs: %v (n=%d)", err, len(mems))
	}
	if !mems[0].Pinned {
		t.Error("Pin: true stored a memory consolidation may rewrite")
	}
}

// TestUpsertPin_DefaultIsUnpinned: pinning is opt-in, and a save that does not
// ask for it must not leave the row exempt from consolidation — that would make
// every agent save a pinned memory by accident.
func TestUpsertPin_DefaultIsUnpinned(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
		"the laptop is not a build host", "mcp", 0.5, nil, UpsertOptions{})
	if err != nil {
		t.Fatalf("UpsertWithOptions: %v", err)
	}

	mems, err := s.GetByIDs(ctx, []string{id})
	if err != nil || len(mems) != 1 {
		t.Fatalf("GetByIDs: %v (n=%d)", err, len(mems))
	}
	if mems[0].Pinned {
		t.Error("a save with no Pin option pinned the memory")
	}
}

// TestUpsertPin_FoldPinsTheExistingRowToo: the row a later consolidation is most
// likely to absorb is the one already in the corpus, not the copy written just
// now — a near-duplicate save that folds keeps the older row as the surviving
// text and links the new one to it, so pinning only the new row would leave the
// fact the caller asked to protect fully consolidatable. Both are pinned.
func TestUpsertPin_FoldPinsTheExistingRowToo(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	existing, _, _, err := s.Upsert(ctx, testProject, "gotcha",
		"the vector identity is compared as an opaque string, never parsed", "mcp", 0.6, nil)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// A near-duplicate that folds into it rather than a distinct memory.
	folded, dup, _, err := s.UpsertWithOptions(ctx, testProject, "gotcha",
		"the vector identity is an opaque compared string, not parsed", "mcp", 0.6, nil, UpsertOptions{Pin: true})
	if err != nil {
		t.Fatalf("UpsertWithOptions: %v", err)
	}
	if dup == "" {
		t.Skip("wording did not fold; the pin-on-existing-row branch was not reached")
	}

	for _, id := range []string{existing, folded} {
		mems, err := s.GetByIDs(ctx, []string{id})
		if err != nil || len(mems) != 1 {
			t.Fatalf("GetByIDs(%s): %v (n=%d)", id, err, len(mems))
		}
		if !mems[0].Pinned {
			t.Errorf("row %s: Pin: true left a row consolidation may rewrite", id)
		}
	}
}

// TestUpsertPin_FoldOnlyFoldPinsTheTargetWithoutStoringTheWording: the
// FoldOnly path stores no new row at all, so the request has nowhere else to
// land. A promotion that pinned nothing would report success and leave the
// memory it was protecting fully consolidatable.
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

	mems, err := s.GetByIDs(ctx, []string{target})
	if err != nil || len(mems) != 1 {
		t.Fatalf("GetByIDs: %v (n=%d)", err, len(mems))
	}
	if !mems[0].Pinned {
		t.Error("a FoldOnly pin left the target unpinned and stored no row of its own")
	}
}
