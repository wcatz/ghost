package supersede

import (
	"context"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// causesTakenStore reports the 'causes' edge as already gone, exactly as a
// concurrent pass would: InvalidateLink moves nothing and says so. It is how the
// "the row carries what the sweep MOVED, not what it was going to move" contract
// is exercised without racing a real second pass.
type causesTakenStore struct {
	*memory.Store
}

func (c causesTakenStore) InvalidateLink(ctx context.Context, sourceID, targetID, relation string) (int64, error) {
	if relation == string(RelationCauses) {
		return 0, nil
	}
	return c.Store.InvalidateLink(ctx, sourceID, targetID, relation)
}

// TestReassessReportsTheObservedSweepNotThePrediction: under --apply the row
// carries what the sweep moved, not what it was going to move. The prediction is
// computed before any write, so a concurrent pass that took the 'causes' edge
// first makes it stale — and a report claiming a deletion that did not happen is
// the one thing this pass cannot be for. The dry run in the same test is what
// proves the prediction was 1 and the applied row says 0.
func TestReassessReportsTheObservedSweepNotThePrediction(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	newer, older := seedEdge(t, store, db,
		"A restore that spanned two spindles took 41 minutes and the row count matched afterwards.",
		"The restore path on one spindle is safe and takes under a minute.")
	if err := store.CreateLink(ctx, older, newer, string(RelationCauses), 0.9, "llm"); err != nil {
		t.Fatal(err)
	}

	dry, dryRows, err := Reassess(ctx, store, NewRelationClassifier(&fakeProvider{resp: "NEITHER"}), "p", false, discardLogger())
	if err != nil {
		t.Fatalf("Reassess (dry): %v", err)
	}
	if dry.CausesWithdrawn != 1 || len(dryRows) != 1 || dryRows[0].CausesSwept != 1 {
		t.Fatalf("dry run = %+v rows %+v, want a prediction of 1 — otherwise the next assertion proves nothing", dry, dryRows)
	}

	res, withdrawn, err := Reassess(ctx, causesTakenStore{Store: store},
		NewRelationClassifier(&fakeProvider{resp: "NEITHER"}), "p", true, discardLogger())
	if err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	if res.CausesWithdrawn != 0 {
		t.Errorf("CausesWithdrawn = %d, want 0: the sweep moved nothing", res.CausesWithdrawn)
	}
	if len(withdrawn) != 1 {
		t.Fatalf("withdrawn = %+v, want one row", withdrawn)
	}
	if withdrawn[0].CausesSwept != 0 {
		t.Errorf("row reports CausesSwept = %d, want the OBSERVED 0 and not the prediction of 1 — "+
			"a row that claims a deletion which did not happen is what this pass exists to stop",
			withdrawn[0].CausesSwept)
	}
	if !withdrawn[0].Written {
		t.Error("the supersedes withdrawal DID land, so the row must say so")
	}
}
