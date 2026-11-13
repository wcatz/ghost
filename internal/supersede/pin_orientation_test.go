package supersede

import (
	"context"
	"testing"
)

// This file is #820: what `ghost_memory_pin` — and the `pin` argument of

// TestPinDoesNotReopenTheEdgeItIsAnEndpointOf is the ordinary pass's half:
func TestPinDoesNotReopenTheEdgeItIsAnEndpointOf(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	// A live version bump: the fix is the newer note, the stale claim the older
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
