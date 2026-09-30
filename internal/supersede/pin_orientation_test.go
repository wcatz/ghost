package supersede

import (
	"context"
	"testing"
)

// This file is #820: what `ghost_memory_pin` — and the `pin` argument of
// `ghost_memory_save` — do to the two signals the supersede pass orients and
// fingerprints pairs by.
//
// A pin is a decision about how a memory is SURFACED, and the row really is
// different afterwards: the `_global` passive bucket orders by `pinned DESC`
// first, the ranking exempts a pinned row from decay, and `ghost resolve`'s
// hold-back refuses to stamp one. None of that is `updated_at`.
//
// Yet `TogglePin` wrote `updated_at = datetime('now')` into the same UPDATE that
// set the flag, and `updated_at` is the pass's chronology (orient) and its change
// detector (skip-if-unchanged) — the two things that decide which direction a
// pair is asked about and whether it is asked at all. So pinning a memory claims
// its note was just written, and the pass believes it.
//
// Two consequences, and both are tested here rather than argued for:
//
//   - the ordinary pass re-asks a pair nobody edited. The edge's stamp is the
//     later of the two endpoints' updated_at as the pass read it, and a pin
//     carries its endpoint past that stamp, so skip-if-unchanged does not hold
//     and the pair is re-classified. #792 exists to make an untouched pair cost
//     nothing, and #779 measured that re-asking a pair is how a correct edge gets
//     withdrawn and re-created on alternating passes — pinning is a paid route to
//     that state on a corpus nobody edited, and pinning is a thing operators do
//     mid-session while a lifecycle phase is mid-flight.
//
//   - the repair pass asks a CYCLE the wrong way round. A cycle has no stored
//     direction to fall back on (that is the whole reason it is one question
//     with a verdict read as a direction), so the direction asked comes from the
//     timestamps — and a pin moves them. The pass would then confirm the
//     backwards edge #641 found in the wild and withdraw the correct one.
//
// The decision is the same one #819 took for `PromoteToGlobal`, and for the same
// reason: `updated_at` moves with the content, and only with the content.

// TestPinDoesNotReopenTheEdgeItIsAnEndpointOf is the ordinary pass's half:
// pinning the OLDER endpoint of a live edge must not re-arm the pair.
//
// The fixture is the quiet case #792 built: a live edge whose stamp is the
// freshness of the text the verdict was made against, with neither endpoint
// touched since. The only write between that state and the pass is the pin, and
// the observable is the classify call count — a pin that moved updated_at bills a
// harness call to re-decide a pair whose two notes have not changed since the edge
// was written.
func TestPinDoesNotReopenTheEdgeItIsAnEndpointOf(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	// A live version bump: the fix is the newer note, the stale claim the older
	// one, and the edge is the one the pass would have written. The texts are the
	// pair the veto and the orientation tests already use, so nothing here is
	// settled by a rule other than the one under test.
	fix := add(t, store, db, "kubernetes now on 1.31", []float32{1, 0, 0}, "2026-09-01 00:00:00")
	stale := add(t, store, db, "kubernetes cluster runs 1.27", []float32{0.98, 0.02, 0}, "2026-01-01 00:00:00")
	// Judged at the freshness of what the verdict was made against, which is the
	// later of the two endpoints' updated_at — so the edge is quiet and the only
	// thing that can wake the pair is a write to an endpoint.
	if err := store.CreateLinkJudged(ctx, fix, stale, string(RelationSupersedes), 0.95, "llm", "2026-09-01 00:00:00"); err != nil {
		t.Fatal(err)
	}

	if err := store.TogglePin(ctx, stale, true); err != nil {
		t.Fatalf("TogglePin: %v", err)
	}

	cls := &supersedesEverything{}
	res, classified, err := Run(ctx, store, cls, "p", 0.9, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(cls.judged) != 0 {
		t.Errorf("the pass judged %d pair-orientation(s) %v, want none: a pin decides how a memory is surfaced and changes none of its text, so the pair the edge already describes is not re-opened by it",
			len(cls.judged), cls.judged)
	}
	if len(classified) != 0 {
		t.Errorf("classified = %+v, want no rows: nothing about this pair was re-decided", classified)
	}
	if res.Candidates != 0 || res.Reclassified != 0 {
		t.Errorf("Candidates=%d Reclassified=%d, want 0/0: the pair's edge stamp is the freshness of the text it was judged against, and pinning a memory is not a change to that text",
			res.Candidates, res.Reclassified)
	}
	// The edge itself, and the direction it runs in, are untouched — the point is
	// that the pass says nothing about the pair, not that it changed it.
	if edges := liveSupersedesEdges(t, store, fix, stale); len(edges) != 1 || edges[0] != [2]string{fix, stale} {
		t.Errorf("live supersedes edges = %v, want exactly [%s %s]: the edge is left exactly as the pin found it",
			edges, fix, stale)
	}
	// The pin itself landed, because a fix that made the pass quiet by dropping
	// the write would pass every assertion above.
	if pinned := onlyMemory(t, store, stale); !pinned.Pinned {
		t.Errorf("pinned = false, want true: TogglePin is still the writer of the flag, and a pass that cannot see a pin is not the pass this is about")
	}
}

// TestPinDoesNotReorientACycleTheRepairPassJudges is the repair pass's half, and
// it is the sharper of the two: a cycle is judged as ONE question and its verdict
// is read as a direction, so the direction the pass asks about decides which of the
// two live edges stands. That direction comes from the timestamps, because a cycle
// is the one shape with no stored direction to fall back on.
//
// So pinning the STALE endpoint of a cycle — the target of the correct edge and the
// source of the backwards one — used to make the pass ask "does the stale note
// supersede the fix?", take SUPERSEDES at its word, KEEP the backwards edge #641
// found in the wild and withdraw the correct one. Nothing about the corpus
// changed; a bookkeeping column did.
func TestPinDoesNotReorientACycleTheRepairPassJudges(t *testing.T) {
	for _, order := range cycleOrders {
		t.Run(order.name, func(t *testing.T) {
			store, db := seed(t)
			ctx := context.Background()
			// seedCycle builds both edges of one pair: the fix supersedes the
			// stale claim (correct) and the stale claim supersedes the fix
			// (#641's damage), which is the state the repair pass exists for.
			stale, fix := seedCycle(t, store, db,
				"bug: the relay stalls on every consumer rebalance",
				"the relay rebalance stall is fixed: pin the consumer", order.fixFirst)

			if err := store.TogglePin(ctx, stale, true); err != nil {
				t.Fatalf("TogglePin: %v", err)
			}

			// supersedesEverything confirms whichever direction it is handed, which
			// is what makes the direction the only variable: the pass stands the
			// edge it asked about, so asking the wrong way round stands the wrong
			// edge.
			cls := &supersedesEverything{}
			var view reassessStore = store
			if order.reversed {
				view = linkOrderStore{Store: store, reversed: true}
			}
			res, _, err := Reassess(ctx, view, cls, "p", true, discardLogger())
			if err != nil {
				t.Fatalf("Reassess: %v", err)
			}
			if len(cls.judged) != 1 {
				t.Fatalf("the pass judged %d pair-orientation(s) %v, want 1: a pair with two live edges is ONE question", len(cls.judged), cls.judged)
			}
			if cls.judged[0] != [2]string{fix, stale} {
				t.Errorf("the cycle was asked about as %v, want [%s %s]: the fix is still the newer note, and pinning a memory is not a rewrite of it",
					cls.judged[0], fix, stale)
			}
			edges := liveEdges(t, store, stale, fix)
			if len(edges) != 1 || edges[0] != [2]string{fix, stale} {
				t.Fatalf("live supersedes edges = %v, want exactly [%s %s]: a pin must not restore the backwards edge while withdrawing the correct one",
					edges, fix, stale)
			}
			if want := outcomeForEdge(t, res.Cyclic[0], edges[0]); res.Cyclic[0].Outcome != want {
				t.Errorf("outcome = %q, want %q", res.Cyclic[0].Outcome, want)
			}
		})
	}
}

// TestPinMovesTheFlagWithoutMovingItsText pins the two facts the fix is made of,
// in the store's own terms, so a future writer that adds the bump back is caught
// here rather than by a supersede test three packages away.
//
// It is a store test wearing a supersede test's name on purpose: the column is
// the store's, and the readers that care (orient and skip-if-unchanged) are in
// another package. The pin is the PRIMARY key of the `_global` passive order, so
// it still reorders what it names without touching the stamp — and the tie-break
// it gives up is a tie-break, which is why the ordering keeps a total answer.
func TestPinMovesTheFlagWithoutMovingItsText(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	note := add(t, store, db, "the ingest service runs Redis 6.2", []float32{1, 0}, "2026-01-01 00:00:00")
	before := onlyMemory(t, store, note)
	if before.Pinned {
		t.Fatalf("the fixture is already pinned, so the pin below would prove nothing")
	}
	if err := store.TogglePin(ctx, note, true); err != nil {
		t.Fatalf("TogglePin: %v", err)
	}
	after := onlyMemory(t, store, note)
	if !after.Pinned {
		t.Errorf("pinned = false, want true: the flag is the whole point of the call")
	}
	if after.Content != before.Content || after.CreatedAt != before.CreatedAt {
		t.Errorf("the pin changed the note itself: content %q→%q, created_at %q→%q", before.Content, after.Content, before.CreatedAt, after.CreatedAt)
	}
	if after.UpdatedAt != before.UpdatedAt {
		t.Errorf("updated_at moved %q → %q on a write that changed no text: that column is the supersede pass's chronology (orient) and its change detector (skip-if-unchanged), and a pin is not a rewrite",
			before.UpdatedAt, after.UpdatedAt)
	}

	// The unpin is the same write, so it is held to the same rule: a flag the
	// operator takes back is not an edit either, and a guard that only watched the
	// pin would leave half the defect in place.
	if err := store.TogglePin(ctx, note, false); err != nil {
		t.Fatalf("TogglePin (unpin): %v", err)
	}
	cleared := onlyMemory(t, store, note)
	if cleared.Pinned {
		t.Errorf("pinned = true after unpin, want false")
	}
	if cleared.UpdatedAt != before.UpdatedAt {
		t.Errorf("updated_at moved %q → %q on the unpin: unpinning is the same decision in the other direction",
			before.UpdatedAt, cleared.UpdatedAt)
	}
}
