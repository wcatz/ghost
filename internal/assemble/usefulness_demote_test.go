package assemble

import (
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func TestRepeatedContradictionIsDemotedBehindClean(t *testing.T) {
	clean := candidate("C1", "proj", "fact", "clean", 0.9)
	clean.Usefulness = memory.UsefulnessEvidence{}
	doubtful := candidate("C2", "proj", "fact", "doubtful", 0.9)
	doubtful.Usefulness = memory.UsefulnessEvidence{Contradicted: 2}
	// Input order is clean then doubtful; after stage provenance doubtful should sink.
	p := &pipeline{
		req:   baseRequest(),
		rows:  []memory.Candidate{clean, doubtful},
		trace: newTrace(baseRequest(), &memory.CandidateSet{Rows: []memory.Candidate{clean, doubtful}}),
	}
	// materialize items as Run would (runValidity does)
	p.items = []Item{itemOf(clean), itemOf(doubtful)}
	// Run provenance stage which owns the demotion.
	runProvenance(p)
	if len(p.rows) != 2 {
		t.Fatalf("rows after provenance = %d, want 2", len(p.rows))
	}
	if p.rows[0].ID != "C1" || p.rows[1].ID != "C2" {
		t.Fatalf("order after demote = %v, want [C1 C2] with C2 demoted to end (already was)", []string{p.rows[0].ID, p.rows[1].ID})
	}
	// Now reverse input: doubtful first, clean second. Demotion should move doubtful behind.
	p2 := &pipeline{
		req:   baseRequest(),
		rows:  []memory.Candidate{doubtful, clean},
		trace: newTrace(baseRequest(), &memory.CandidateSet{Rows: []memory.Candidate{doubtful, clean}}),
	}
	p2.items = []Item{itemOf(doubtful), itemOf(clean)}
	runProvenance(p2)
	if p2.rows[0].ID != "C1" || p2.rows[1].ID != "C2" {
		t.Fatalf("demoted order = %v, want clean first", []string{p2.rows[0].ID, p2.rows[1].ID})
	}
	if p2.items[0].ID != "C1" || p2.items[1].ID != "C2" {
		t.Fatalf("items not kept aligned with rows: %v", []string{p2.items[0].ID, p2.items[1].ID})
	}
}

func TestSingleContradictionIsNotDemoted(t *testing.T) {
	clean := candidate("C1", "proj", "fact", "clean", 0.9)
	doubtful := candidate("C2", "proj", "fact", "once", 0.9)
	doubtful.Usefulness = memory.UsefulnessEvidence{Contradicted: 1}
	p := &pipeline{
		req:   baseRequest(),
		rows:  []memory.Candidate{doubtful, clean},
		trace: newTrace(baseRequest(), &memory.CandidateSet{Rows: []memory.Candidate{doubtful, clean}}),
	}
	p.items = []Item{itemOf(doubtful), itemOf(clean)}
	runProvenance(p)
	if p.rows[0].ID != "C2" {
		t.Fatalf("single contradiction demoted unexpectedly: order %v", []string{p.rows[0].ID, p.rows[1].ID})
	}
}

func TestFlaggedAloneDoesNotDemote(t *testing.T) {
	clean := candidate("C1", "proj", "fact", "clean", 0.9)
	flagged := candidate("C2", "proj", "fact", "flagged", 0.9)
	flagged.Usefulness = memory.UsefulnessEvidence{Flagged: 3}
	p := &pipeline{
		req:   baseRequest(),
		rows:  []memory.Candidate{flagged, clean},
		trace: newTrace(baseRequest(), &memory.CandidateSet{Rows: []memory.Candidate{flagged, clean}}),
	}
	p.items = []Item{itemOf(flagged), itemOf(clean)}
	runProvenance(p)
	if p.rows[0].ID != "C2" {
		t.Fatalf("flagged alone demoted unexpectedly: order %v", []string{p.rows[0].ID, p.rows[1].ID})
	}
}
