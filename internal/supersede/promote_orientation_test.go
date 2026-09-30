package supersede

import (
	"context"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// This file is #807: what `ghost_memory_promote` — the one caller of
// memory.Store.PromoteToGlobal in production — does to the two signals the
// supersede pass orients and fingerprints pairs by.
//
// A promotion is a MOVE. It changes which project a memory belongs to and
// nothing else: not its text, not its created_at, not its category, not its
// importance. Yet PromoteToGlobal wrote `updated_at = datetime('now')` into the
// same UPDATE that moved it, and `updated_at` is the pass's chronology (orient)
// and its change detector (skip-if-unchanged) — the two things that decide which
// direction a pair is asked about and whether it is asked at all. So promoting a
// memory claims its note was just written, and the pass believes it.
//
// Two consequences, and both are tested here rather than argued for:
//
//   - the ordinary pass re-asks a pair nobody edited. The edge's stamp is the
//     later of the two endpoints' updated_at as the pass read it, and a
//     promotion carries one endpoint past that stamp, so skip-if-unchanged does
//     not hold and the pair is re-classified. #792 exists to make an untouched
//     pair cost nothing, and #779 measured that re-asking a pair is how a
//     correct edge gets withdrawn and re-created on alternating passes — the
//     promotion is a paid way to reach that state on a corpus nobody edited.
//
//   - the repair pass asks a CYCLE the wrong way round. A cycle has no stored
//     direction to fall back on (that is the whole reason it is one question
//     with a verdict read as a direction), so the direction asked comes from
//     the timestamps — and a promotion moves them. The pass would then confirm
//     the backwards edge #641 found in the wild and withdraw the correct one.
//
// Neither is hypothetical: an agent calls `ghost_memory_promote` mid-session,
// moving a LIVE memory out of the project with its links and its id intact,
// while a stop hook's lifecycle phase or a manual pass reads the same graph.
// (`ghost reflect --promote-globals` is a different write — it folds a
// candidate into an existing `_global` row or inserts a new one, and the
// project's own row goes with the consolidation — so it is not this shape.)

// TestPromotionDoesNotReopenTheEdgeItIsAnEndpointOf is the ordinary pass's half:
// promoting the OLDER endpoint of a live edge must not re-arm the pair.
//
// The fixture is the quiet case #792 built: a live edge whose stamp is the
// freshness of the text the verdict was made against, with neither endpoint
// touched since. The only write between that state and the pass is the
// promotion, and the observable is the classify call count — a promotion that
// moved updated_at bills a harness call to re-decide a pair whose two notes
// have not changed since the edge was written.
func TestPromotionDoesNotReopenTheEdgeItIsAnEndpointOf(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	// A live version bump: the fix is the newer note, the stale claim the older
	// one, and the edge is the one the pass would have written. The texts are
	// the pair the veto and the orientation tests already use, so nothing here
	// is settled by a rule other than the one under test.
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
// half, and it is the sharper of the two: a cycle is judged as ONE question and
// its verdict is read as a direction, so the direction the pass asks about
// decides which of the two live edges stands. That direction comes from the
// timestamps, because a cycle is the one shape with no stored direction to fall
// back on.
//
// So promoting the STALE endpoint of a cycle — the target of the correct edge
// and the source of the backwards one — used to make the pass ask "does the
// stale note supersede the fix?", take SUPERSEDES at its word, KEEP the
// backwards edge #641 found in the wild and withdraw the correct one. Nothing
// about the corpus changed; a bookkeeping column did.
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
			// which is what makes the direction the only variable: the pass
			// stands the edge it asked about, so asking the wrong way round
			// stands the wrong edge.
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
// made of, in the store's own terms, so a future writer that adds the bump back
// is caught here rather than by a supersede test three packages away.
//
// It is a store test wearing a supersede test's name on purpose: the column is
// the store's, and the two readers that care (orient and skip-if-unchanged) are
// in another package.
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
