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
		legCandidate("dual", 3, 4, 1.0),
		legCandidate("vecTop", -1, 1, 0.9),
		legCandidate("vecDrop", -1, 5, 0.8),
		legCandidate("ftsTop", 0, -1, 0.7),
		legCandidate("ftsDrop", 5, -1, 0.6),
	}
	set := &memory.CandidateSet{Rows: rows, Legs: aliveLegs()}
	res := run(t, &fakeRetriever{set: set}, consensusQueryRequest())
	if ids := itemIDs(res.Items); !eq(ids, []string{"dual", "vecTop", "ftsTop"}) {
		t.Fatalf("items = %v, want dual/top single-leg rows admitted", ids)
	}
	if d := traceDecision(res, "vecDrop", stageLegConsensus); d == nil || d.Reason != reasonLegConsensus {
		t.Errorf("vecDrop should be dropped as single-leg non-top: %+v", d)
	}
	if d := traceDecision(res, "ftsDrop", stageLegConsensus); d == nil || d.Reason != reasonLegConsensus {
		t.Errorf("ftsDrop should be dropped as single-leg non-top: %+v", d)
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

func traceDecision(res Result, id, stage string) *Decision {
	for i := range res.Trace.Decisions {
		d := &res.Trace.Decisions[i]
		if d.ID == id && d.Stage == stage {
			return d
		}
	}
	return nil
}

// stageTraceFor already exists? ensure helper name in tests
var _ = stageTraceFor
