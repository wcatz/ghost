package bench

import (
	"math"
	"testing"
)

// TestPairedBootstrapIsDeterministic: a gate that prints a different interval on
// every run is a gate nobody can read, and a seed that changes with the Go
// release makes the number in docs/benchmarks.md unreproducible. Same input, same
// interval, and the observed mean is reported unchanged whatever the resampling
// does.
func TestPairedBootstrapIsDeterministic(t *testing.T) {
	diffs := []float64{0.1, -0.2, 0.3, 0.0, 0.05, -0.1, 0.2, 0.4, -0.3, 0.15}
	lo1, hi1, mean1 := bootstrapMeanCI(diffs, 2000, 7)
	lo2, hi2, mean2 := bootstrapMeanCI(diffs, 2000, 7)
	if lo1 != lo2 || hi1 != hi2 || mean1 != mean2 {
		t.Errorf("bootstrap is not deterministic: (%.6f, %.6f, %.6f) then (%.6f, %.6f, %.6f)", lo1, hi1, mean1, lo2, hi2, mean2)
	}
	// A different seed must move the interval — otherwise the resampling is not
	// doing anything and the interval is a constant.
	lo3, hi3, _ := bootstrapMeanCI(diffs, 2000, 8)
	if lo3 == lo1 && hi3 == hi1 {
		t.Errorf("a different seed gave the same interval (%.6f, %.6f): the resamples are not being drawn", lo1, hi1)
	}
	var wantMean float64
	for _, d := range diffs {
		wantMean += d
	}
	wantMean /= float64(len(diffs))
	if math.Abs(mean1-wantMean) > 1e-12 {
		t.Errorf("mean = %.9f, want the observed mean %.9f", mean1, wantMean)
	}
	if lo1 > mean1 || hi1 < mean1 {
		t.Errorf("interval (%.6f, %.6f) does not contain the observed mean %.6f", lo1, hi1, mean1)
	}
}

// TestPairedBootstrapCoversTheMeanItShould covers the two cases the gate reads:
// a consistent positive difference (fusion helping on every query) must exclude
// zero, and pure noise must not. A percentile interval that always contained the
// mean and often contained zero would make the gate unfalsifiable.
func TestPairedBootstrapCoversTheMeanItShould(t *testing.T) {
	// 40 queries, every one better by 0.05: the interval is far above zero.
	helped := make([]float64, 40)
	for i := range helped {
		helped[i] = 0.05
	}
	lo, hi, mean := bootstrapMeanCI(helped, 5000, 3)
	if lo <= 0 {
		t.Errorf("a difference positive on every query gave an interval reaching zero: (%.4f, %.4f)", lo, hi)
	}
	if math.Abs(mean-0.05) > 1e-12 {
		t.Errorf("mean = %.6f, want 0.05", mean)
	}

	// 40 queries alternating +0.1 / -0.1: zero mean, and the interval must span
	// it, because nothing here is a real difference.
	noise := make([]float64, 40)
	for i := range noise {
		if i%2 == 0 {
			noise[i] = 0.1
		} else {
			noise[i] = -0.1
		}
	}
	lo, hi, mean = bootstrapMeanCI(noise, 5000, 3)
	if lo > 0 || hi < 0 {
		t.Errorf("alternating noise gave an interval that excludes zero: (%.4f, %.4f)", lo, hi)
	}
	if math.Abs(mean) > 1e-12 {
		t.Errorf("noise mean = %.9f, want 0", mean)
	}
}

// TestPairedNDCGRefusesToPairDifferentQuerySets: the pairing is positional, so two
// runs of the same length over different queries would produce a confident,
// meaningless interval. The check is the only thing standing between a refactor
// that reorders the query set and a gate that quietly starts measuring nothing.
func TestPairedNDCGRefusesToPairDifferentQuerySets(t *testing.T) {
	a := Result{Condition: CondHybrid, PerQuery: []QueryScore{{Name: "q1", NDCG: 1}, {Name: "q2", NDCG: 0.5}}}
	b := Result{Condition: CondVector, PerQuery: []QueryScore{{Name: "q1", NDCG: 0.5}, {Name: "q2", NDCG: 0.5}}}
	diffs, err := pairedNDiff(a, b)
	if err != nil {
		t.Fatalf("pairing the same query names in the same order: %v", err)
	}
	if len(diffs) != 2 || diffs[0] != 0.5 || diffs[1] != 0 {
		t.Errorf("diffs = %v, want [0.5 0] — the difference is per query, not a difference of means", diffs)
	}

	c := Result{Condition: CondVector, PerQuery: []QueryScore{{Name: "q2", NDCG: 0.5}, {Name: "q1", NDCG: 0.5}}}
	if _, err := pairedNDiff(a, c); err == nil {
		t.Error("paired the same queries in a different order without an error")
	}
	d := Result{Condition: CondVector, PerQuery: []QueryScore{{Name: "q1", NDCG: 0.5}}}
	if _, err := pairedNDiff(a, d); err == nil {
		t.Error("paired results of different lengths without an error")
	}

	// The same names in the same order is the one case that pairs, and the
	// difference is per query rather than a difference of means.
	e := Result{Condition: CondVector, PerQuery: []QueryScore{{Name: "q1", NDCG: 0.5}, {Name: "q2", NDCG: 0.0}}}
	diffs, err = pairedNDiff(a, e)
	if err != nil {
		t.Fatalf("pairing matching query sets: %v", err)
	}
	if len(diffs) != 2 || diffs[0] != 0.5 || diffs[1] != 0.5 {
		t.Errorf("diffs = %v, want [0.5 0.5]", diffs)
	}
}

// TestBootstrapMeanCIRefusesToInventAnInterval: the resample count is a
// parameter so a caller can vary it, and a count of zero used to index an empty
// slice. There is no interval to report from zero resamples, and the only
// defensible answer is the sample itself rather than the extremes of a
// distribution that was never drawn.
func TestBootstrapMeanCIRefusesToInventAnInterval(t *testing.T) {
	diffs := []float64{0.1, -0.2, 0.3, 0.0, 0.05, -0.1, 0.2, 0.4, -0.3, 0.15}
	want := func() float64 {
		var sum float64
		for _, d := range diffs {
			sum += d
		}
		return sum / float64(len(diffs))
	}()
	for _, resamples := range []int{0, -1} {
		lo, hi, mean := bootstrapMeanCI(diffs, resamples, 1)
		if lo != mean || hi != mean || mean != want {
			t.Errorf("resamples=%d gave (%.6f, %.6f, %.6f), want the observed mean %.6f three times",
				resamples, lo, hi, mean, want)
		}
	}
	// One resample is not degenerate in the same way — it is a distribution of
	// one, so the interval is that resample and both edges are it. Asserted only
	// so the shape is stated rather than left to be discovered.
	if lo, hi, _ := bootstrapMeanCI(diffs, 1, 1); lo != hi {
		t.Errorf("resamples=1 gave a two-edged interval (%.6f, %.6f)", lo, hi)
	}
	// An empty sample is the other degenerate input, and it is not a panic either.
	if lo, hi, mean := bootstrapMeanCI(nil, 100, 1); lo != 0 || hi != 0 || mean != 0 {
		t.Errorf("empty sample gave (%.3f, %.3f, %.3f), want zeros", lo, hi, mean)
	}
}

// TestFusionGateDecision pins the rule the regression gate applies, on intervals
// the committed dataset does not produce. The two that matter are the first pair:
// the old gate was a comparison of point estimates, so it rejected an interval
// whose evidence says fusion is not behind, and that is the brittleness #561 is
// about. The gate has to accept it and still reject the case below it, or the
// tolerance is a way of switching the assertion off.
func TestFusionGateDecision(t *testing.T) {
	cases := []struct {
		name string
		ci   PairDiff
		want bool // true = accepted
	}{
		{"measured: ahead of vector by a hair", PairDiff{Leg: CondVector, Mean: 0.0171, Lo: 0.0022, Hi: 0.0328, Queries: 220}, true},
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

// TestFusionToleranceIsNotAnAbsolution: the tolerance has to be small enough
// that a real ranking regression trips it, and the way to know that is to state
// what it costs in queries. At n=220, flipping one query from a perfect 1.0 to a
// zero costs 1/220 = 0.0045 of the mean, so the tolerance is 4.4 queries — enough
// to absorb a re-grading, far too little to absorb fusion actually losing.
func TestFusionToleranceIsNotAnAbsolution(t *testing.T) {
	perQuery := 1.0 / 220.0
	queries := fusionTolerance / perQuery
	if queries < 3 || queries > 8 {
		t.Errorf("tolerance %.2f is %.1f queries' worth of NDCG at n=220; want between 3 and 8, so it absorbs a re-grading and not a regression", fusionTolerance, queries)
	}
}
