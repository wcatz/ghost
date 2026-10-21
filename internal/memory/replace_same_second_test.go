package memory

import (
	"context"
	"testing"
)

// #1745: the consolidation window's own stamp and a memory's created_at are
// both second-precision, so a memory written in the SAME SECOND the window
// opened is marked concurrent by the fail-safe comparison and every proposal
// that claims it is refused. The refusal is deliberate — the pass cannot prove
// it read the row before the window opened — but nothing changed, and counting
// the refusal as a concurrent change told the operator that a memory moved
// when it did not.
//
// The store's own contract is the split: a claim over a row the pass read and
// that has not moved since is a SAME-SECOND refusal, and a claim over a row
// that really moved is a CHANGE, however the row was stamped.
func TestReplaceNonManualCountsASameSecondRefusalApartFromAChange(t *testing.T) {
	ctx := context.Background()

	t.Run("a row written in the window's own second is held back, not reported as a change", func(t *testing.T) {
		s := testStore(t)
		aID, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: "the deploy host is alpha", Source: "mcp", Importance: 0.6, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("Create A: %v", err)
		}
		a := getOne(t, s, aID)
		fed := corpusRows(t, s, aID)
		emitted := []Memory{{Category: "fact", Content: a.Content, Importance: a.Importance, Tags: []string{}}}

		// The window's stamp IS the row's own second.
		preserved, dropped, deleted, appliedByCategory, err := s.ReplaceNonManual(ctx, testProject, emitted, a.CreatedAt, fed)
		if err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}

		if dropped.SameSecond != 1 {
			t.Errorf("dropped.SameSecond = %d, want 1: the claim was refused because the row shares the window's second", dropped.SameSecond)
		}
		if dropped.Changed != 0 {
			t.Errorf("dropped.Changed = %d, want 0: no row changed while the pass ran", dropped.Changed)
		}
		if dropped.Total() != 1 {
			t.Errorf("dropped.Total() = %d, want 1: a held-back proposal is still a refused one", dropped.Total())
		}
		if deleted != 0 {
			t.Errorf("deleted = %d, want 0: a refused claim takes nothing", deleted)
		}
		if len(appliedByCategory) != 0 {
			t.Errorf("appliedByCategory = %v, want empty: the proposal landed nowhere", appliedByCategory)
		}
		if len(preserved) != 1 || preserved[0] != aID {
			t.Errorf("preserved = %v, want [%s]: the row is still live and a later pass has to merge it", preserved, aID)
		}
		got := getOne(t, s, aID)
		if got.Content != a.Content || got.UpdatedAt != a.UpdatedAt {
			t.Errorf("the row = %q (updated %q), want %q (updated %q): a refused claim overwrites nothing",
				got.Content, got.UpdatedAt, a.Content, a.UpdatedAt)
		}
	})

	t.Run("a row that really moved is a change, whatever second it was written in", func(t *testing.T) {
		s := testStore(t)
		aID, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: "the deploy host is alpha", Source: "mcp", Importance: 0.6, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("Create A: %v", err)
		}
		a := getOne(t, s, aID)
		fed := corpusRows(t, s, aID)
		emitted := []Memory{{Category: "fact", Content: a.Content, Importance: a.Importance, Tags: []string{}}}

		// The edit lands while the harness is answering.
		moved := "the deploy host is bravo (moved)"
		if err := s.UpdateMemory(ctx, testProject, aID, &moved, nil, nil, nil); err != nil {
			t.Fatalf("UpdateMemory A: %v", err)
		}

		// The window's stamp is still the row's own second: the edit is what
		// makes the claim void, not the second the row was written in.
		preserved, dropped, _, _, err := s.ReplaceNonManual(ctx, testProject, emitted, a.CreatedAt, fed)
		if err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}
		if dropped.Changed != 1 {
			t.Errorf("dropped.Changed = %d, want 1: the row really changed under the pass", dropped.Changed)
		}
		if dropped.SameSecond != 0 {
			t.Errorf("dropped.SameSecond = %d, want 0: an edited row is not a same-second save", dropped.SameSecond)
		}
		if len(preserved) != 1 || preserved[0] != aID {
			t.Errorf("preserved = %v, want [%s]: the edited row survives", preserved, aID)
		}
		if got := getOne(t, s, aID); got.Content != moved {
			t.Errorf("the row = %q, want the concurrent edit %q kept", got.Content, moved)
		}
	})

	t.Run("a row older than the window is not concurrent at all", func(t *testing.T) {
		s := testStore(t)
		aID, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: "the deploy host is alpha", Source: "mcp", Importance: 0.6, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("Create A: %v", err)
		}
		setAgesDaysAgo(t, s, aID, 1)
		a := getOne(t, s, aID)
		fed := corpusRows(t, s, aID)
		emitted := []Memory{{Category: "fact", Content: a.Content, Importance: a.Importance, Tags: []string{}}}

		consolidatedSince, err := s.CurrentTimestamp(ctx)
		if err != nil {
			t.Fatalf("CurrentTimestamp: %v", err)
		}
		preserved, dropped, _, appliedByCategory, err := s.ReplaceNonManual(ctx, testProject, emitted, consolidatedSince, fed)
		if err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}
		if dropped.Total() != 0 {
			t.Errorf("dropped = %+v, want no drops: the row is older than the window", dropped)
		}
		if len(preserved) != 0 {
			t.Errorf("preserved = %v, want none: nothing was concurrent", preserved)
		}
		if len(appliedByCategory) != 1 || appliedByCategory["fact"] != 1 {
			t.Errorf("appliedByCategory = %v, want {fact:1}: the proposal was applied", appliedByCategory)
		}
	})
}
