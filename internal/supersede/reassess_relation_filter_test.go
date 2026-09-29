package supersede

import (
	"context"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestReassessSweepIsScopedToTheCausesRelation pins the RELATION half of the
// 'causes' sweep, where the rule is actually decided.
//
// Every other test of the sweep seeds a 'causes' edge and nothing else, so
// `liveCausesPairs`'s `l.Relation == string(RelationCauses)` is never the reason
// a row is in the map. Its only reader was the memory-layer test, and this
// function is not a memory-layer read: it is the reassess pass's own sweep,
// over the ids the deletion can actually reach.
//
// A 'related' edge standing in a causes edge's place is the competing row, and
// it has to be a pair of its own for the filter to be observable at all. The
// prediction is a membership test on the (older, newer) PAIR, so a 'related'
// edge added beside a real 'causes' edge on the same pair is invisible either
// way — the set holds one key and the filter is not what produced it. What
// separates them is a pair whose only edge from the older note is a 'related'
// one: the linker's relation, written on similarity alone and never judged,
// joining the same two memories and asserting nothing about whether the older
// note is still current.
//
// Counted as a causes pair, that pair reports a second graph row the operator
// is about to delete — and the apply cannot make that deletion, because
// InvalidateLink is scoped to the relation too. So the dry run promises a
// deletion the repair does not perform, which is the one thing this pass's
// prediction exists to prevent. In the other direction, treating the row as
// swept would move a live edge of a relation the pass has no verdict about.
func TestReassessSweepIsScopedToTheCausesRelation(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	// Pair A: the ordinary shape — a supersedes edge to judge, and the
	// contradicting 'causes' edge a pre-#686 rubric could have left behind.
	newerA, olderA := seedEdge(t, store, db,
		"A restore that spanned two spindles took 41 minutes and the row count matched afterwards.",
		"The restore path on one spindle is safe and takes under a minute.")
	if err := store.CreateLink(ctx, olderA, newerA, string(RelationCauses), 0.9, "llm"); err != nil {
		t.Fatal(err)
	}

	// Pair B: identical in every way that reaches the classifier, except that
	// the competing edge on the pair is 'related'/'auto' — what the linker
	// writes — and there is NO 'causes' edge at all.
	newerB, olderB := seedEdge(t, store, db,
		"The nightly compaction finishes in 12 minutes on the primary.",
		"The nightly compaction takes under an hour on the primary.")
	if err := store.CreateLink(ctx, olderB, newerB, "related", 0.9, "auto"); err != nil {
		t.Fatal(err)
	}

	// Both edges are judgements to make, and neither loader nor sweep may see
	// the third relation either way.
	dry, dryRows, err := Reassess(ctx, store, NewRelationClassifier(&fakeProvider{resp: "NEITHER"}), "p", false, discardLogger())
	if err != nil {
		t.Fatalf("Reassess (dry): %v", err)
	}
	if dry.Loaded != 2 {
		t.Fatalf("Loaded = %d, want 2: only the 'supersedes'/'llm' edges are judgements to make", dry.Loaded)
	}
	if len(dryRows) != 2 {
		t.Fatalf("dry run rows = %+v, want one per judged edge", dryRows)
	}
	// ONE causes row across the two pairs. This is the assertion that fails
	// when the relation comparison is gone: pair B's 'related' row would then
	// be counted, and the operator reads this number before running the repair.
	if dry.CausesWithdrawn != 1 {
		t.Errorf("dry run predicted %d causes edge(s), want 1 — a 'related' row standing in a causes edge's place is not one, and this is the figure an operator decides the repair on",
			dry.CausesWithdrawn)
	}
	if got := sweptByRow(dryRows)[olderA]; got != 1 {
		t.Errorf("pair A's row predicts %d sweep(s), want 1: it has a live 'causes' edge", got)
	}
	if got := sweptByRow(dryRows)[olderB]; got != 0 {
		t.Errorf("pair B's row predicts %d sweep(s), want 0: the only competing edge on that pair is 'related', and InvalidateLink could not have deleted it",
			got)
	}

	res, rows, err := Reassess(ctx, store, NewRelationClassifier(&fakeProvider{resp: "NEITHER"}), "p", true, discardLogger())
	if err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	// The apply moves the one real 'causes' edge, so the observed count agrees
	// with the prediction. Had the prediction been 2, this is where the
	// disagreement would show.
	if res.CausesWithdrawn != 1 {
		t.Errorf("apply swept %d causes edge(s), want 1", res.CausesWithdrawn)
	}
	if len(rows) != 2 {
		t.Fatalf("apply rows = %+v, want one per withdrawn edge", rows)
	}
	for _, r := range rows {
		if !r.Written {
			t.Errorf("row %s→%s is not marked written, so the NEITHER verdict withdrew nothing", r.NewerID, r.OlderID)
		}
	}
	if got := sweptByRow(rows)[olderA]; got != 1 {
		t.Errorf("pair A's row reports %d swept, want 1", got)
	}
	if got := sweptByRow(rows)[olderB]; got != 0 {
		t.Errorf("pair B's row reports %d swept, want 0", got)
	}

	// And the graph state, read from the store rather than the report: the
	// 'causes' edge is gone, the 'related' edge is not, and both supersedes
	// edges were withdrawn by the verdict.
	if liveRelations(t, store, olderA)[string(RelationCauses)] {
		t.Error("pair A's 'causes' edge is still live, so the sweep moved nothing")
	}
	if liveRelations(t, store, olderB)[string(RelationSupersedes)] {
		t.Error("pair B's supersedes edge is still live, so the NEITHER verdict withdrew nothing")
	}
	if !liveRelations(t, store, olderB)["related"] {
		t.Error("pair B's 'related' edge is gone: the sweep is scoped to the 'causes' relation and must not move a row of another")
	}
}

// sweptByRow indexes each row's observed/predicted sweep count by the older id,
// which is what identifies a pair on a report row.
func sweptByRow(rows []WithdrawnEdge) map[string]int {
	out := map[string]int{}
	for _, r := range rows {
		out[r.OlderID] = r.CausesSwept
	}
	return out
}

// liveRelations reports which relations the store still holds a LIVE edge for on
// any pair touching id. It reads the store rather than a fixture list, because
// the claim under test is about graph state after the repair, not about what the
// repair reported.
func liveRelations(t *testing.T, store *memory.Store, id string) map[string]bool {
	t.Helper()
	links, err := store.GetLinks(context.Background(), id)
	if err != nil {
		t.Fatalf("GetLinks: %v", err)
	}
	out := map[string]bool{}
	for _, l := range links {
		out[l.Relation] = true
	}
	return out
}
