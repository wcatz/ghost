package supersede

import (
	"context"
	"testing"
)

// TestTheCausesWriteIsNotGuarded is the asymmetry in the guarded writer, pinned
// from the pass's side so it cannot be "tidied" away by a later change that
// guards both relations for the sake of symmetry.
//
// A 'supersedes' edge is guarded because a pair live in BOTH directions demotes
// BOTH endpoints and neither edge withdraws the other (#778, #806). A 'causes'
// edge demotes nothing — the ranking guards read only 'supersedes' — so the
// second direction of a 'causes' pair costs nothing, while refusing it costs a
// CLASSIFY CALL ON EVERY PASS FOREVER: the pass's direction override is built
// from live supersedes edges, so a pair whose 'causes' edge runs against the
// timestamps is re-proposed in the flipped direction each pass, answered CAUSES
// again, and would be refused again. A refusal that never converges is worse
// than the contradiction it prevents.
//
// What this leaves is a pre-existing gap with its own issue: nothing reconciles a
// live 'causes' edge's direction with the timestamps, and only a supersedes pair
// is ever cache-skipped, so such a pair is re-asked every pass whatever this
// writer does. The fix for that is to give a live 'causes' edge the same
// direction override and the same never-cache-skip a live 'supersedes' edge has —
// which is a change to the pass's reads, not to this writer.
func TestTheCausesWriteIsNotGuarded(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	// A pair whose 'causes' edge runs AGAINST the timestamps: the note that is
	// currently newer is the edge's SOURCE, and a 'causes' edge is written older
	// to newer, so the pass will want the reverse of what is there. That is the
	// only shape in which the guard would have refused.
	older := add(t, store, db, "the restore path on one spindle is safe and fast", []float32{1, 0, 0}, "2026-01-01 00:00:00")
	newer := add(t, store, db, "the restore is being rewritten to run on one spindle", []float32{0.98, 0.02, 0}, "2026-09-01 00:00:00")
	if err := store.CreateLink(ctx, newer, older, string(RelationCauses), 0.9, "llm"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}

	res, _, err := Run(ctx, store, &causesEverything{}, "p", 0.9, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ReverseLive != 0 {
		t.Errorf("ReverseLive = %d, want 0: the guarded writer must not be used for a 'causes' edge, or this pair is refused on every pass forever (see the comment above)", res.ReverseLive)
	}
	if res.CausesCreated != 1 {
		t.Errorf("CausesCreated = %d, want 1: the verdict is acted on like any other", res.CausesCreated)
	}
	// What the pass wrote, read from the graph: the 'causes' edge in the
	// direction the timestamps give, alongside the one already there. The pair is
	// left holding both, which is the state the comment above accepts and the
	// follow-up issue owns.
	links, err := store.GetLinks(ctx, older)
	if err != nil {
		t.Fatalf("GetLinks: %v", err)
	}
	var causes []memory2Link
	for _, l := range links {
		if l.Relation == string(RelationCauses) {
			causes = append(causes, memory2Link{source: l.SourceID, target: l.TargetID})
		}
	}
	if len(causes) != 2 {
		t.Fatalf("live 'causes' edges touching the older endpoint = %v, want both directions: this is the accepted state, and the assertion is here so a change to it is a decision rather than a drift", causes)
	}
	want := memory2Link{source: older, target: newer}
	found := false
	for _, c := range causes {
		if c == want {
			found = true
		}
	}
	if !found {
		t.Errorf("live 'causes' edges = %v, want the timestamp direction [%s -> %s] among them", causes, older, newer)
	}
}

// memory2Link is a {source, target} pair, so the assertion above can print what
// it found without depending on memory.Link's field order or names.
type memory2Link struct{ source, target string }

// causesEverything answers CAUSES to every pair. CAUSES is the verdict that
// makes the older note the cause and the newer one its effect, and it is the one
// that writes a 'causes' edge — the relation the guarded writer is not used for.
type causesEverything struct{}

func (c *causesEverything) ClassifyBatch(_ context.Context, pairs []Candidate) ([]Relation, error) {
	out := make([]Relation, len(pairs))
	for i := range pairs {
		out[i] = RelationCauses
	}
	return out, nil
}
