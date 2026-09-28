package bench

import (
	"strings"
	"testing"
)

// TestFormatRankedStateReadsItsFooterFromAPrintedRun: the aggregate loop skips
// any run with fewer than three conditions, but the footer then read
// runs[0].Result[0].Queries unconditionally — so a slice whose FIRST entry is
// incomplete and whose second is complete printed a row and then panicked with an
// index out of range. RankedStateRun is exported, so a hand-built slice reaches
// this, and the function's own comment says a formatter that panics on anything
// else is one the next caller cannot use.
//
// The fix is to remember which run was actually printed and take the query count
// from that one, so the footer and the table above it describe the same run.
func TestFormatRankedStateReadsItsFooterFromAPrintedRun(t *testing.T) {
	three := func(q int) []Result {
		return []Result{
			{Condition: CondFTS, Queries: q, NDCG10: 0.5},
			{Condition: CondVector, Queries: q, NDCG10: 0.7},
			{Condition: CondHybrid, Queries: q, NDCG10: 0.8},
		}
	}
	// Labels carry the leg weight, so the assertions can name a run and check
	// the skipped one is absent.
	runs := []RankedStateRun{
		// FIRST is incomplete: the loop skips it, so it is never printed.
		{Config: RankedStateConfig{Label: "vec=0.90"}, Result: nil},
		{Config: RankedStateConfig{Label: "vec=0.70"}, Result: three(17)},
		{Config: RankedStateConfig{Label: "vec=0.30"}, Result: three(17)},
	}

	// The old footer read runs[0].Result[0] and panicked here.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("FormatRankedState panicked on a slice whose first run is incomplete: %v", r)
		}
	}()
	out := FormatRankedState(runs)

	if !strings.Contains(out, "17 graded queries") {
		t.Errorf("the footer did not take the query count from the run it printed:\n%s", out)
	}
	// And the AGGREGATE table must not carry the skipped run, or the footer is
	// describing a row the table above it does not contain. Scoped to that table:
	// the per-probe grid further down lists every run's label by design, because a
	// run with an incomplete aggregate can still have per-probe answers, and that
	// section is a different claim.
	aggregate := out
	if i := strings.Index(out, "answer rank by probe"); i >= 0 {
		aggregate = out[:i]
	}
	if strings.Contains(aggregate, "vec=0.90") {
		t.Errorf("the incomplete run appears in the aggregate table the loop skipped it from:\n%s", aggregate)
	}
	// The two runs that WERE complete are both there, so the table is not passing
	// by printing one row and skipping the rest.
	for _, label := range []string{"vec=0.70", "vec=0.30"} {
		if !strings.Contains(aggregate, label) {
			t.Errorf("the complete run %s is missing from the aggregate table:\n%s", label, aggregate)
		}
	}
}

// TestFormatRankedStateSaysSoWhenNothingIsComplete: every run short of three
// conditions is the degenerate input the footer used to panic on, and the
// function already has an answer for it. Asserted so the "no complete
// configuration" path is reached by construction rather than by luck.
func TestFormatRankedStateSaysSoWhenNothingIsComplete(t *testing.T) {
	runs := []RankedStateRun{
		{Config: RankedStateConfig{Label: "vec=0.90"}, Result: []Result{{Condition: CondHybrid, Queries: 4}}},
		{Config: RankedStateConfig{Label: "vec=0.70"}, Result: nil},
	}
	out := FormatRankedState(runs)
	if !strings.Contains(out, "no complete configuration to report") {
		t.Errorf("a slice with no complete run did not say so:\n%s", out)
	}
}
