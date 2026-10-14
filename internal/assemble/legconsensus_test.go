package assemble

import (
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func legCandidate(id string, ftsRank, vecRank int, score float64) memory.Candidate {
	c := candidate(id, "proj", "fact", "content "+id, score)
	c.FTSRank = ftsRank
	c.VectorRank = vecRank
	return c
}

func aliveLegs() map[string]memory.LegStatus {
	return map[string]memory.LegStatus{
		"fts":    {Applicable: true, Available: true},
		"vector": {Applicable: true, Available: true},
	}
}

func consensusQueryRequest() Request {
	req := baseRequest()
	req.Query = "database configuration"
	req.Condition = memory.CondHybrid
	req.Budget = Budget{MaxItems: 10}
	req.RelevanceCutoff = 0
	return req
}

func TestLegConsensusKeepsDualAndTopSingleLegs(t *testing.T) {
	rows := []memory.Candidate{
		// The top-ranked row is always admitted even though one leg found it
		// past the ceiling; the stage must never empty a query block.
		legCandidate("topSingle", -1, 9, 1.0),
		legCandidate("dual", 3, 4, 0.9),
		legCandidate("vecTop", -1, 1, 0.8),
		legCandidate("vecDrop", -1, 5, 0.7),
		legCandidate("ftsTop", 0, -1, 0.6),
		legCandidate("ftsDrop", 5, -1, 0.5),
	}
	pinned := legCandidate("pinned", -1, 7, 0.75)
	pinned.Pinned = true
	rows = append(rows, pinned)
	reserved := legCandidate("reserved", 9, -1, 0.65)
	reserved.KeywordReserved = true
	rows = append(rows, reserved)
	set := &memory.CandidateSet{Rows: rows, Legs: aliveLegs()}
	res := run(t, &fakeRetriever{set: set}, consensusQueryRequest())
	if ids := itemIDs(res.Items); !eq(ids, []string{"topSingle", "dual", "vecTop", "ftsTop", "pinned", "reserved"}) {
		t.Fatalf("items = %v, want the top row, dual rows, top single-leg rows and the pinned/reserved rows admitted", ids)
	}
	if d := traceDecision(res, "topSingle", stageLegConsensus); d != nil {
		t.Errorf("the top-ranked row should never be dropped by the leg-consensus stage: %+v", d)
	}
	for _, id := range []string{"vecDrop", "ftsDrop"} {
		if d := traceDecision(res, id, stageLegConsensus); d == nil || d.Reason != reasonLegConsensus {
			t.Errorf("%s should be dropped as single-leg non-top: %+v", id, d)
		}
	}
	for _, id := range []string{"pinned", "reserved"} {
		if d := traceDecision(res, id, stageLegConsensus); d != nil {
			t.Errorf("%s must not be dropped by the leg-consensus stage: %+v", id, d)
		}
	}
}

func TestLegConsensusSkipsWhenOneLegUnavailable(t *testing.T) {
	rows := []memory.Candidate{
		legCandidate("dual", 3, 4, 1.0),
		legCandidate("vecDrop", -1, 5, 0.8),
	}
	legs := aliveLegs()
	legs["vector"] = memory.LegStatus{Applicable: true, Available: false, Err: "down"}
	set := &memory.CandidateSet{Rows: rows, Legs: legs}
	res := run(t, &fakeRetriever{set: set}, consensusQueryRequest())
	if ids := itemIDs(res.Items); !eq(ids, []string{"dual", "vecDrop"}) {
		t.Fatalf("items = %v, want all rows kept when one leg is down", ids)
	}
	st := stageTraceFor(res, stageLegConsensus)
	if st.In != st.Out || len(st.DroppedIDs) != 0 {
		t.Errorf("stage = in %d out %d dropped %v, want pass-through", st.In, st.Out, st.DroppedIDs)
	}
}

// TestLegConsensusSkipsWithoutAnEmbedder is the no-embedder shape of the same
// rule: the vector leg is not applicable, so the consensus rule cannot apply
// and the stage must leave the keyword-only block untouched rather than cut
// single-leg rows against a missing leg.
func TestLegConsensusSkipsWithoutAnEmbedder(t *testing.T) {
	rows := []memory.Candidate{
		legCandidate("ftsFirst", 0, -1, 1.0),
		legCandidate("ftsFar", 9, -1, 0.8),
	}
	legs := map[string]memory.LegStatus{
		"fts":    {Applicable: true, Available: true},
		"vector": {Applicable: false},
	}
	set := &memory.CandidateSet{Rows: rows, Legs: legs}
	res := run(t, &fakeRetriever{set: set}, consensusQueryRequest())
	if ids := itemIDs(res.Items); !eq(ids, []string{"ftsFirst", "ftsFar"}) {
		t.Fatalf("items = %v, want all keyword rows kept without an embedder", ids)
	}
	st := stageTraceFor(res, stageLegConsensus)
	if st.In != st.Out || len(st.DroppedIDs) != 0 {
		t.Errorf("stage = in %d out %d dropped %v, want pass-through", st.In, st.Out, st.DroppedIDs)
	}
}

func traceDecision(res Result, id, stage string) *Decision {
	for i := range res.Trace.Decisions {
		d := &res.Trace.Decisions[i]
		if d.ID == id && d.Stage == stage {
			return d
		}
	}
	return nil
}
