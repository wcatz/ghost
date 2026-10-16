package assemble

import (
	"context"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// TestQueryModeStandInSurvivesLegConsensus: the near-duplicate removal keeps
// the LATER row of a pair in place of the earlier one. Here the earlier row O
// is the top row and both legs found it; its later restatement N shares no
// keyword with the query, so only the vector leg found it, below its top rank.
// Judged on its own evidence N is single-leg noise and leg consensus drops it,
// so the answer would hold neither row of the pair, with no loss recorded
// against the pair. The stand-in is admitted on the evidence of the row it
// displaced.
func TestQueryModeStandInSurvivesLegConsensus(t *testing.T) {
	ctx := context.Background()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := memory.NewStore(db, nil)
	if err := s.EnsureProject(ctx, "proj", "/src/proj", "proj"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	add := func(content string, day int, vec []float32) string {
		t.Helper()
		id, err := s.Create(ctx, "proj", memory.Memory{
			Category: "fact", Content: content, Source: "manual", Importance: 0.7,
		})
		if err != nil {
			t.Fatalf("Create(%q): %v", content, err)
		}
		stamp := time.Date(2026, 9, day, 10, 0, 0, 0, time.UTC).Format(memory.StoredStampLayout)
		if _, err := db.Exec(`UPDATE memories SET created_at = ?, updated_at = ? WHERE id = ?`, stamp, stamp, id); err != nil {
			t.Fatalf("stamp %s: %v", id, err)
		}
		if err := s.StoreEmbedding(ctx, id, vec, "test-model"); err != nil {
			t.Fatalf("StoreEmbedding(%q): %v", content, err)
		}
		return id
	}
	older := add("database configuration for the replica refresh runs hourly", 20, []float32{1, 0, 0, 0})
	other := add("database configuration quorum zebra", 21, []float32{0.9, 0.3, 0, 0})
	newer := add("the replica refresh cadence is sixty minutes", 24, []float32{0.8, 0.5, 0, 0})
	if err := s.CreateLink(ctx, older, newer, "related", 0.95, "auto"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}

	req := Request{
		ProjectID: "proj",
		Query:     "database configuration",
		QueryVec:  []float32{1, 0, 0, 0},
		Source:    SourceSearch,
		Budget:    Budget{MaxItems: 5},
		Condition: memory.CondHybrid,
		Now:       time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
	}
	res := run(t, s, req)
	ids := itemIDs(res.Items)
	if !hasID(ids, other) {
		t.Fatalf("items = %v: the fixture is wrong, %s should be admitted", ids, other)
	}
	if !hasID(ids, older) && !hasID(ids, newer) {
		var why []string
		for _, d := range res.Trace.Decisions {
			if d.ID == older || d.ID == newer {
				why = append(why, d.ID+":"+d.Stage+":"+d.Reason)
			}
		}
		t.Errorf("items = %v: neither row of the pair (%s, %s) is in the answer: %v", ids, older, newer, why)
	}
}

// TestCutoffJudgesAStandInByTheRowItDisplaced: the relevance cutoff compares a
// row's Base with a share of the top row's. A stand-in whose own Base is below
// the floor is kept when the row it displaced clears it.
func TestCutoffJudgesAStandInByTheRowItDisplaced(t *testing.T) {
	top := legCandidate("top", 0, 0, 1.0)
	top.Base = 1.0
	standIn := legCandidate("standIn", 1, 1, 0.5)
	standIn.Base = 0.2
	weak := legCandidate("weak", 2, 2, 0.4)
	weak.Base = 0.2
	displaced := legCandidate("displaced", 0, 0, 1.1)
	displaced.Base = 1.1
	set := &memory.CandidateSet{
		Rows:          []memory.Candidate{top, standIn, weak},
		Legs:          aliveLegs(),
		DroppedLosers: []memory.DroppedLoser{{Candidate: displaced, LostTo: []string{"standIn"}, StandIns: []string{"standIn"}}},
	}
	req := consensusQueryRequest()
	req.RelevanceCutoff = 0.63
	res := run(t, &fakeRetriever{set: set}, req)
	if ids := itemIDs(res.Items); !eq(ids, []string{"top", "standIn"}) {
		t.Fatalf("items = %v, want the stand-in kept and the unrelated weak row cut", ids)
	}
}

// TestLegConsensusJudgesAStandInByTheRowItDisplaced is the same rule at the
// leg-consensus gate, and its control: a removed row recorded as a plain rank
// loss (no stand-in) lends nothing.
func TestLegConsensusJudgesAStandInByTheRowItDisplaced(t *testing.T) {
	for _, tc := range []struct {
		name     string
		standIns []string
		want     []string
	}{
		{"stand-in", []string{"standIn"}, []string{"top", "standIn"}},
		{"rank-loss", nil, []string{"top"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set := &memory.CandidateSet{
				Rows: []memory.Candidate{
					legCandidate("top", 0, 0, 1.0),
					legCandidate("standIn", -1, 4, 0.5),
				},
				Legs:          aliveLegs(),
				DroppedLosers: []memory.DroppedLoser{{Candidate: legCandidate("displaced", 1, 1, 0.9), LostTo: []string{"standIn"}, StandIns: tc.standIns}},
			}
			res := run(t, &fakeRetriever{set: set}, consensusQueryRequest())
			if ids := itemIDs(res.Items); !eq(ids, tc.want) {
				t.Fatalf("items = %v, want %v", ids, tc.want)
			}
		})
	}
}
