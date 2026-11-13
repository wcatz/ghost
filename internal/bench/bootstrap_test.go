package bench

import (
	"testing"

	"github.com/wcatz/ghost/internal/config"
)

// TestFusionGateDecision pins the rule the regression gate applies. Only the first
// case is a real interval — the rest are hand-built so the rule is pinned on inputs
// the committed dataset does not produce, which is how a real regression is told
// from a hypothetical one. The two that matter are the first pair: the old gate was
// a comparison of point estimates, so it rejected an interval whose evidence says
// fusion is not behind, and that is the brittleness #561 is about. The gate has to
// accept it and still reject the case below it, or the tolerance is a way of
// switching the assertion off.
func TestFusionGateDecision(t *testing.T) {
	cases := []struct {
		name string
		ci   PairDiff
		want bool // true = accepted
	}{
		// The measured interval over the committed v3 dataset, so "measured" in
		// the case name is a fact the test can be held to: TestBenchRegressionFloors
		// logs +0.0178 [+0.0033, +0.0327] from the same code and the same seed pair.
		{"measured: ahead of vector by a hair", PairDiff{Leg: CondVector, Mean: 0.0178, Lo: 0.0033, Hi: 0.0327, Queries: 220}, true},
		{"point estimate dipped below zero, the interval still excludes a real loss",
			PairDiff{Leg: CondVector, Mean: -0.001, Lo: -0.015, Hi: 0.010, Queries: 220}, true},
		{"a consistent loss inside the tolerance", PairDiff{Leg: CondFTS, Mean: -0.015, Lo: -0.019, Hi: -0.008, Queries: 220}, true},
		{"exactly at the tolerance is not a pass", PairDiff{Leg: CondFTS, Mean: -0.03, Lo: -0.02, Hi: -0.01, Queries: 220}, false},
		{"a real regression", PairDiff{Leg: CondVector, Mean: -0.061, Lo: -0.078, Hi: -0.043, Queries: 220}, false},
		{"a large regression", PairDiff{Leg: CondHybrid, Mean: -0.4, Lo: -0.5, Hi: -0.3, Queries: 220}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := fusionGate(c.ci)
			if accepted := err == nil; accepted != c.want {
				if c.want {
					t.Errorf("fusionGate rejected an acceptable interval %+.4f [%+.4f, %+.4f]: %v", c.ci.Mean, c.ci.Lo, c.ci.Hi, err)
				} else {
					t.Errorf("fusionGate accepted a regression %+.4f [%+.4f, %+.4f]", c.ci.Mean, c.ci.Lo, c.ci.Hi)
				}
			}
		})
	}
	// The old rule, stated so the change is checkable: it compared point
	// estimates, so it failed the second case above on the mean alone.
	old := PairDiff{Leg: CondVector, Mean: -0.001, Lo: -0.015, Hi: 0.010, Queries: 220}
	if old.Mean >= 0 {
		t.Error("the old point-estimate rule would have accepted this case, so it is not the rule the issue described")
	}
}

// TestPairingRefusesMismatchedRunsAndSmallSamples pins the edges of the paired
// bootstrap: runs over different queries are refused rather than paired by
// position, and a sample too small to resample reports a point, not an interval.
func TestPairingRefusesMismatchedRunsAndSmallSamples(t *testing.T) {
	a := Result{Condition: "a", PerQuery: []QueryScore{{Name: "q1", NDCG: 1}, {Name: "q2", NDCG: 0.5}}}
	for name, b := range map[string]Result{
		"length": {Condition: "b", PerQuery: a.PerQuery[:1]},
		"names":  {Condition: "b", PerQuery: []QueryScore{{Name: "q1"}, {Name: "other"}}},
	} {
		if _, err := CompareFusion(a, b); err == nil {
			t.Errorf("%s mismatch: runs were paired", name)
		}
	}
	if lo, hi, mean := bootstrapMeanCI([]float64{0.25}, 100); lo != 0.25 || hi != 0.25 || mean != 0.25 {
		t.Errorf("one-sample CI = [%v, %v] mean %v, want a point", lo, hi, mean)
	}
	if lo, hi, mean := bootstrapMeanCI(nil, 100); lo != 0 || hi != 0 || mean != 0 {
		t.Errorf("empty CI = [%v, %v] mean %v, want zeros", lo, hi, mean)
	}
	if got := armIndex("no-such-arm"); got != -1 {
		t.Errorf("armIndex of an unknown arm = %d, want -1", got)
	}
	q := Query{ProjectID: "p", Text: "t"}
	if got := ContextRequest(q, ContextInstant()); got.RelevanceCutoff != config.DefaultRelevanceCutoff || got.NoAnswerCosine != config.DefaultNoAnswerCosine {
		t.Errorf("ContextRequest carries cutoff %v bar %v, want the shipped defaults", got.RelevanceCutoff, got.NoAnswerCosine)
	}
	if got := contextRequestWithCutoff(q, ContextInstant(), 0.3); got.RelevanceCutoff != 0.3 || got.NoAnswerCosine != 0 {
		t.Errorf("cutoff-only request = cutoff %v bar %v, want 0.3 and no bar", got.RelevanceCutoff, got.NoAnswerCosine)
	}
	if len(SweepGrid()) == 0 {
		t.Error("SweepGrid is empty")
	}
}
