package supersede

import (
	"context"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// This file is #807: what `ghost_memory_promote` — the one caller of

// TestPromotionDoesNotReopenTheEdgeItIsAnEndpointOf is the ordinary pass's half:
func TestPromotionDoesNotReopenTheEdgeItIsAnEndpointOf(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	// A live version bump: the fix is the newer note, the stale claim the older
	fix := add(t, store, db, "kubernetes now on 1.31", []float32{1, 0, 0}, "2026-09-01 00:00:00")
	stale := add(t, store, db, "kubernetes cluster runs 1.27", []float32{0.98, 0.02, 0}, "2026-01-01 00:00:00")
	// Judged at the freshness of what the verdict was made against, which is
	// the later of the two endpoints' updated_at — so the edge is quiet and the
	// only thing that can wake the pair is a write to an endpoint.
	if err := store.CreateLinkJudged(ctx, fix, stale, string(RelationSupersedes), 0.95, "llm", "2026-09-01 00:00:00"); err != nil {
		t.Fatal(err)
	}

	if err := store.PromoteToGlobal(ctx, "p", stale); err != nil {
		t.Fatalf("PromoteToGlobal: %v", err)
	}

	cls := &supersedesEverything{}
	res, classified, err := Run(ctx, store, cls, "p", 0.9, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(cls.judged) != 0 {
		t.Errorf("the pass judged %d pair-orientation(s) %v, want none: a promotion moves a memory between projects and changes no text, so the pair the edge already describes is not re-opened by it",
			len(cls.judged), cls.judged)
	}
	if len(classified) != 0 {
		t.Errorf("classified = %+v, want no rows: nothing about this pair was re-decided", classified)
	}
	if res.Candidates != 0 || res.Reclassified != 0 {
		t.Errorf("Candidates=%d Reclassified=%d, want 0/0: the pair's edge stamp is the freshness of the text it was judged against, and a promotion is not a change to that text",
			res.Candidates, res.Reclassified)
	}
	// The edge itself, and the direction it runs in, are untouched — the point
	// is that the pass says nothing about the pair, not that it changed it.
	if edges := liveSupersedesEdges(t, store, fix, stale); len(edges) != 1 || edges[0] != [2]string{fix, stale} {
		t.Errorf("live supersedes edges = %v, want exactly [%s %s]: the edge is left exactly as the promotion found it",
			edges, fix, stale)
	}
}

// TestPromotionDoesNotReorientACycleTheRepairPassJudges is the repair pass's
func TestPromotionDoesNotReorientACycleTheRepairPassJudges(t *testing.T) {
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

			if err := store.PromoteToGlobal(ctx, "p", stale); err != nil {
				t.Fatalf("PromoteToGlobal: %v", err)
			}

			// supersedesEverything confirms whichever direction it is handed,
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
				t.Errorf("the cycle was asked about as %v, want [%s %s]: the newer note is still the newer note, and a promotion is a move rather than a rewrite",
					cls.judged[0], fix, stale)
			}
			edges := liveEdges(t, store, stale, fix)
			if len(edges) != 1 || edges[0] != [2]string{fix, stale} {
				t.Fatalf("live supersedes edges = %v, want exactly [%s %s]: a promotion must not restore the backwards edge while withdrawing the correct one",
					edges, fix, stale)
			}
			if want := outcomeForEdge(t, res.Cyclic[0], edges[0]); res.Cyclic[0].Outcome != want {
				t.Errorf("outcome = %q, want %q", res.Cyclic[0].Outcome, want)
			}
		})
	}
}

// TestPromotionMovesTheMemoryWithoutMovingItsText pins the two facts the fix is
func TestPromotionMovesTheMemoryWithoutMovingItsText(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	note := add(t, store, db, "the ingest service runs Redis 6.2", []float32{1, 0}, "2026-01-01 00:00:00")
	before := onlyMemory(t, store, note)
	if before.ProjectID != "p" {
		t.Fatalf("the fixture is in project %q, want p", before.ProjectID)
	}
	if err := store.PromoteToGlobal(ctx, "p", note); err != nil {
		t.Fatalf("PromoteToGlobal: %v", err)
	}
	after := onlyMemory(t, store, note)
	if after.ProjectID != memory.GlobalProjectID {
		t.Errorf("project_id = %q, want %q: the promotion did not move the memory", after.ProjectID, memory.GlobalProjectID)
	}
	if after.Content != before.Content || after.CreatedAt != before.CreatedAt {
		t.Errorf("the promotion changed the note itself: content %q→%q, created_at %q→%q", before.Content, after.Content, before.CreatedAt, after.CreatedAt)
	}
	if after.UpdatedAt != before.UpdatedAt {
		t.Errorf("updated_at moved %q → %q on a write that changed no text: that column is the supersede pass's chronology (orient) and its change detector (skip-if-unchanged), and a move is not a rewrite",
			before.UpdatedAt, after.UpdatedAt)
	}
}

// onlyMemory reads one row back by id and fails the test unless the store
// returned exactly it, so a fixture that silently lost its memory cannot make
// the assertions below pass against a zero value.
func onlyMemory(t *testing.T, store *memory.Store, id string) memory.Memory {
	t.Helper()
	mems, err := store.GetByIDs(context.Background(), []string{id})
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	if len(mems) != 1 {
		t.Fatalf("GetByIDs(%s) returned %d rows, want 1", id, len(mems))
	}
	return mems[0]
}
