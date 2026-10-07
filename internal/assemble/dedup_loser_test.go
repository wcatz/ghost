package assemble

import (
	"reflect"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// droppedLoser is a near-duplicate loser the retriever removed: a candidate that
// is NOT in the set's Rows, with the ids it lost to.
func droppedLoser(c memory.Candidate, lostTo ...string) memory.DroppedLoser {
	return memory.DroppedLoser{Candidate: c, LostTo: lostTo}
}

func sessionStartWithLoser() (*fakeRetriever, Request) {
	set := passiveSet(globalCandidate("g1", 0.9), globalCandidate("g2", 0.5))
	set.DroppedLosers = []memory.DroppedLoser{droppedLoser(globalCandidate("gdup", 0.8), "g1")}
	req := passiveRequest()
	req.Budget.Slices = []Slice{{
		Bucket: memory.GlobalProjectID, MaxItems: 8, OverFetch: 16,
		Order: "pinned_importance_updated", DemotionThreshold: 0.85, DropDemotedLosers: true,
	}}
	return &fakeRetriever{set: set}, req
}

// TestDroppedNearDuplicateLoserIsInTheTrace (#894): a loser the retriever removed
// never enters CandidateSet.Rows, so before this nothing in the trace named it.
// Stage 6 files it as a drop with its reason and the row it lost to.
func TestDroppedNearDuplicateLoserIsInTheTrace(t *testing.T) {
	f, req := sessionStartWithLoser()
	res := run(t, f, req)

	var got *Decision
	for i, d := range res.Trace.Decisions {
		if d.ID == "gdup" {
			got = &res.Trace.Decisions[i]
		}
	}
	if got == nil {
		t.Fatalf("the removed loser is absent from Trace.Decisions: %+v", res.Trace.Decisions)
	}
	if got.Stage != stageDedup || got.Reason != reasonNearDuplicate || got.Kept || got.ProjectID != memory.GlobalProjectID {
		t.Errorf("decision = %+v, want a dropped %s/%s decision for the _global row", *got, stageDedup, reasonNearDuplicate)
	}
	if !reflect.DeepEqual(got.Against, []string{"g1"}) {
		t.Errorf("Against = %v, want [g1]", got.Against)
	}
	if got.Before != 0.8 {
		t.Errorf("Before = %v, want the loser's score 0.8", got.Before)
	}
	var st StageTrace
	for _, s := range res.Trace.Stages {
		if s.Stage == stageDedup {
			st = s
		}
	}
	if !reflect.DeepEqual(st.DroppedIDs, []string{"gdup"}) || st.In != 3 || st.Out != 2 {
		t.Errorf("dedup stage = in %d out %d dropped %v, want 3 -> 2 dropping gdup", st.In, st.Out, st.DroppedIDs)
	}
	// Which rows are shown is unchanged: the loser was never an answer row.
	if ids := itemIDs(res.Items); !reflect.DeepEqual(ids, []string{"g1", "g2"}) {
		t.Errorf("items = %v, want [g1 g2]", ids)
	}
}

// TestDroppedNearDuplicateLoserIsInTheRetrievalRecord: the record is built from
// the trace's decisions, so the removed row is counted as a dropped verdict at
// the dedup stage.
func TestDroppedNearDuplicateLoserIsInTheRetrievalRecord(t *testing.T) {
	f, req := sessionStartWithLoser()
	sink := &recordingSink{}
	req.Record = sink
	run(t, f, req)

	var found bool
	for _, v := range sink.last(t).Verdicts {
		if v.ID == "gdup" {
			found = true
			if v.Kept || v.Stage != stageDedup || v.Reason != reasonNearDuplicate {
				t.Errorf("verdict = %+v, want dropped at dedup with reason near_duplicate", v)
			}
		}
	}
	if !found {
		t.Errorf("the retrieval record never mentions the removed loser: %+v", sink.last(t).Verdicts)
	}
}

// TestDedupedTallyComesFromTheTrace: the session-start header's Deduped count is
// the trace's own stage 6 count, not a residual guessed from the store's eligible
// count. CountedAgainst must not add a second, independent count on top.
func TestDedupedTallyComesFromTheTrace(t *testing.T) {
	f, req := sessionStartWithLoser()
	res := run(t, f, req)
	tally := CountsFor(res.Trace, memory.GlobalProjectID, 2)
	if tally.Deduped != 1 || tally.Withheld != 0 || tally.RankedOut != 0 {
		t.Fatalf("tally = %+v, want exactly one Deduped and nothing withheld or ranked out", tally)
	}
	// The store holds 3 eligible rows and the window fetched all of them: the
	// trace already accounts for every one, so nothing more is attributed.
	got := tally.CountedAgainst(3, 0, 16)
	if got.Deduped != 1 || got.Total() != 3 {
		t.Errorf("after CountedAgainst: %+v, want Deduped 1 and Total 3", got)
	}
}

// TestExplainReportsADroppedLoserAsNotIncluded: explain is a projection of the
// same trace, so a removed loser is a candidate that was not included, with the
// reason and near_duplicate_of naming its winner.
func TestExplainReportsADroppedLoserAsNotIncluded(t *testing.T) {
	set := factSet(nil, candidate("w", "proj", "fact", "database configuration winner", 0.9))
	set.DroppedLosers = []memory.DroppedLoser{
		droppedLoser(candidate("l", "proj", "fact", "database configuration loser", 0.8), "w"),
	}
	res := run(t, &fakeRetriever{set: set}, withExplain(baseRequest()))
	row := explainRowByID(t, res.Explain, "l")
	if row.Included {
		t.Error("the removed loser is marked included")
	}
	if !reflect.DeepEqual(row.NearDuplicateOf, []string{Token("w")}) {
		t.Errorf("near_duplicate_of = %v, want [%s]", row.NearDuplicateOf, Token("w"))
	}
	if row.Reason == "" || row.Reason == "not in the answer, and no stage recorded why" {
		t.Errorf("reason = %q, want the stage 6 verdict", row.Reason)
	}
	if !explainRowByID(t, res.Explain, "w").Included {
		t.Error("the winner is not included")
	}
}
