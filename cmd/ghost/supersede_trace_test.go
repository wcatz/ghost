package main

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/supersede"
)

// TestSupersedeTraceSeparatesNeverProposedFromRejected: the diagnosis of a miss
// names both halves — a pair candidate generation dropped and a pair the
// classifier rejected — so a reader can tell which stage to fix.
func TestSupersedeTraceSeparatesNeverProposedFromRejected(t *testing.T) {
	t.Setenv("GHOST_SUPERSEDE_TRACE", "1")
	res := supersede.Result{NotProposed: []supersede.RejectedPair{
		{NewerID: "aaaaaaaabb", OlderID: "ccccccccdd", Reason: "below the cosine floor and no lexical replacement"},
	}}
	classified := []supersede.Classified{{
		Candidate: supersede.Candidate{NewerID: "eeeeeeeeff", OlderID: "1111111122"},
		Relation:  supersede.RelationNeither,
	}}
	out := supersedeTraceLines(res, classified)
	for _, want := range []string{
		"aaaaaaaa -> cccccccc not proposed: below the cosine floor",
		"eeeeeeee -> 11111111 proposed: classifier verdict neither",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("trace %q does not contain %q", out, want)
		}
	}
	if got := supersedeTraceLines(res, classified); got != out {
		t.Errorf("trace is not stable: %q then %q", out, got)
	}
}

// TestSupersedeTraceIsOffByDefault: the knob prints nothing unless asked, so an
// ordinary report is byte-identical to what it was before.
func TestSupersedeTraceIsOffByDefault(t *testing.T) {
	t.Setenv("GHOST_SUPERSEDE_TRACE", "")
	if got := supersedeTraceLines(supersede.Result{Candidates: 2}, nil); got != "" {
		t.Errorf("trace without the knob = %q, want empty", got)
	}
}
