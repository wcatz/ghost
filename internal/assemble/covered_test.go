package assemble

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestRunWithholdsRowsTheCallerReportsCovered: a row Request.Covered reports is
// withheld before the budget, with its own reason, so the bucket counts it as
// withheld and the freed slot goes to the next row.
func TestRunWithholdsRowsTheCallerReportsCovered(t *testing.T) {
	f := &fakeRetriever{set: passiveSet(
		projectCandidate("p1", 0.9),
		globalCandidate("g1", 0.5), globalCandidate("g2", 0.4), globalCandidate("g3", 0.3),
	)}
	req := passiveRequest()
	req.Covered = func(c memory.Candidate) bool { return c.ID == "g1" }
	res := run(t, f, req)

	var globals []string
	for _, it := range res.Items {
		if it.ID == "g1" {
			t.Fatalf("covered row g1 was admitted: %v", itemIDs(res.Items))
		}
		if it.Bucket == memory.GlobalProjectID {
			globals = append(globals, it.ID)
		}
	}
	if len(globals) != 2 || globals[0] != "g2" || globals[1] != "g3" {
		t.Errorf("globals = %v, want [g2 g3] (the slot g1 left goes to the next row)", globals)
	}
	tally := CountsFor(res.Trace, memory.GlobalProjectID, len(globals))
	if tally.Withheld != 1 || tally.Reason != ReasonHostInstructions {
		t.Errorf("global tally = %+v, want 1 withheld for %q", tally, ReasonHostInstructions)
	}
}

// TestWithheldNoteNamesHostInstructions: a bucket whose every row was covered
// says so, rather than calling the rows untrustworthy.
func TestWithheldNoteNamesHostInstructions(t *testing.T) {
	note := BucketTally{Withheld: 2, Reason: ReasonHostInstructions}.WithheldNote()
	if !strings.Contains(note, "instruction files") {
		t.Errorf("WithheldNote = %q, want it to name the host's instruction files", note)
	}
}
