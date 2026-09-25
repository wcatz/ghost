package memory

import (
	"context"
	"testing"
)

// TestUpsertFoldOnlyDoesNotInsertTheIncomingText is issue #544.
//
// _global accumulated 68 redundant rows in 19 clusters — one "gouroboros PR
// workflow" fact had nine reflection-sourced paraphrases. Every project scope
// had zero such clusters, because _global is written by promotion from every
// project's reflect, while project memories are written once by one project.
//
// Upsert's normal fold strengthens the target and then inserts the incoming
// text as its own row linked as a duplicate. That is right for a save: the
// caller explicitly asked for this text to be stored, so the new wording is
// kept. It is wrong for promotion, where the same fact arriving from another
// project's reflection is a fresh paraphrase of something _global already
// knows, and a row per paraphrase is pure bloat that still occupies a window
// slot and feeds resolve and supersede.
func TestUpsertFoldOnlyDoesNotInsertTheIncomingText(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	first, _, _, err := s.Upsert(ctx, testProject, "fact",
		"the gouroboros PR workflow pushes from the laptop clone", "reflection", 0.7, nil)
	if err != nil {
		t.Fatalf("first Upsert: %v", err)
	}

	// A paraphrase, as a second project's reflect would produce.
	id, dupOf, score, err := s.UpsertWithOptions(ctx, testProject, "fact",
		"the gouroboros PR workflow pushes from the laptop clone too",
		"reflection", 0.7, nil, UpsertOptions{FoldOnly: true})
	if err != nil {
		t.Fatalf("fold-only Upsert: %v", err)
	}

	if dupOf != first {
		t.Errorf("duplicateOf = %q, want the existing memory %q — fold-only must still find it", dupOf, first)
	}
	if id != first {
		t.Errorf("id = %q, want the existing memory %q — fold-only must not mint a new row", id, first)
	}
	if score <= 0 {
		t.Errorf("score = %v, want the match score to be reported", score)
	}

	all, err := s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("project holds %d memories, want 1 — fold-only inserted the paraphrase: %+v", len(all), all)
	}
	if all[0].Content != "the gouroboros PR workflow pushes from the laptop clone" {
		t.Errorf("content = %q, want the original wording preserved", all[0].Content)
	}
}

// TestUpsertFoldOnlyStillStrengthens: folding is not discarding. The incoming
// text is evidence the fact is worth keeping, so the existing row must be
// strengthened exactly as the ordinary fold path strengthens it — otherwise
// promotion would silently lose the signal that made the duplicate worth
// noticing.
func TestUpsertFoldOnlyStillStrengthens(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	first, _, _, err := s.Upsert(ctx, testProject, "fact",
		"the gouroboros PR workflow pushes from the laptop clone", "reflection", 0.7, nil)
	if err != nil {
		t.Fatalf("first Upsert: %v", err)
	}
	before, err := s.GetByIDs(ctx, []string{first})
	if err != nil || len(before) == 0 {
		t.Fatalf("GetByIDs before: %v", err)
	}

	if _, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
		"the gouroboros PR workflow pushes from the laptop clone too",
		"reflection", 0.7, nil, UpsertOptions{FoldOnly: true}); err != nil {
		t.Fatalf("fold-only Upsert: %v", err)
	}

	after, err := s.GetByIDs(ctx, []string{first})
	if err != nil || len(after) == 0 {
		t.Fatalf("GetByIDs after: %v", err)
	}
	if after[0].Importance <= before[0].Importance {
		t.Errorf("importance = %v, want it strengthened above %v — fold-only must not discard the duplicate",
			after[0].Importance, before[0].Importance)
	}
	if after[0].AccessCount <= before[0].AccessCount {
		t.Errorf("access_count = %d, want it strengthened above %d", after[0].AccessCount, before[0].AccessCount)
	}
}

// TestUpsertFoldOnlyInsertsWhenThereIsNoDuplicate: fold-only changes what
// happens on a match, not on a miss. A genuinely new global memory must still
// be written, or promotion would silently drop facts _global has never seen.
func TestUpsertFoldOnlyInsertsWhenThereIsNoDuplicate(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, dupOf, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
		"cardano epoch boundaries are driven by the slot length", "reflection", 0.7, nil,
		UpsertOptions{FoldOnly: true})
	if err != nil {
		t.Fatalf("fold-only Upsert on a miss: %v", err)
	}
	if id == "" {
		t.Fatal("fold-only returned no id for a memory _global has never seen")
	}
	if dupOf != "" {
		t.Errorf("duplicateOf = %q, want empty on a miss", dupOf)
	}

	all, err := s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("project holds %d memories, want the new one written", len(all))
	}
}

// TestUpsertDefaultStillInsertsTheDuplicateRow: fold-only is opt-in and scoped
// to promotion. Ordinary saves must keep inserting the caller's text as its own
// row, because the caller explicitly asked for that text to be stored.
func TestUpsertDefaultStillInsertsTheDuplicateRow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if _, _, _, err := s.Upsert(ctx, testProject, "fact",
		"the gouroboros PR workflow pushes from the laptop clone", "reflection", 0.7, nil); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}
	second, _, _, err := s.Upsert(ctx, testProject, "fact",
		"the gouroboros PR workflow pushes from the laptop clone too", "reflection", 0.7, nil)
	if err != nil {
		t.Fatalf("second Upsert: %v", err)
	}

	all, err := s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("project holds %d memories, want 2 — the default fold must keep the caller's wording", len(all))
	}
	if second == "" {
		t.Error("default fold returned no id for the inserted row")
	}
}
