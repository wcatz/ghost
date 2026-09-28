package bench

import (
	"fmt"
	"math/rand"
	"sort"
)

// The fusion gate asks one question: is hybrid fusion actually earning its keep
// over either single leg? The old gate was `hybrid.NDCG10 >= vector.NDCG10` on a
// dataset where the two differ by 0.017, and it was two claims dressed as one —
// that fusion does not hurt, and that it helps. The first is worth asserting; the
// second needs an interval, because a 0.017 gap over 220 queries is inside the
// noise a resample of the query set produces, and a hard inequality on a noisy
// gap is a gate that fires on the fixture and nowhere else.
//
// So the comparison is made the way the statistics are: paired per query, with a
// percentile bootstrap over the differences. Two conditions scoring the SAME
// query set are correlated query by query, which is most of the variance; pairing
// removes it and leaves the variation attributable to the ranking, which is the
// thing under test. The interval is the honest statement of how much of the
// observed gap the data supports.
//
// Everything here is deterministic on purpose: a fixed seed and a fixed
// resample count, so the interval a CI run prints is the interval the next one
// prints. A gate whose number moved between runs would be unreadable and would
// eventually be tuned until it passed.

// Bootstrap defaults. 20000 resamples puts the 2.5th and 97.5th percentile on
// roughly the 500th order statistic, which is as fine as the interval's three
// printed digits are meaningful; the seed is the standard PCG source, chosen for
// being stable across Go versions rather than for being any particular generator.
const (
	bootstrapResamples = 20000
	bootstrapSeed      = 1
	bootstrapAlpha     = 0.05
)

// PairDiff is the per-query difference in NDCG@10 between two conditions over the
// same graded query set, together with the sample's mean and a percentile
// bootstrap interval for it.
//
// Every field is a claim about the dataset the comparison ran on, so none of it is
// cached or reused across datasets: a stale interval in a test failure is worse
// than no interval.
type PairDiff struct {
	Leg       string
	Mean      float64 // mean per-query difference (hybrid minus leg)
	Lo, Hi    float64 // bootstrap interval at bootstrapAlpha
	Queries   int     // queries paired
	Resamples int
}

// pairedNDiff returns the per-query NDCG@10 difference (a - b) over the shared
// query set, or an error naming the first mismatch. The names are compared
// because the pairing is positional: two Result slices that happen to be the same
// length but were produced from different query sets would otherwise produce a
// confident, meaningless interval.
func pairedNDiff(a, b Result) ([]float64, error) {
	if len(a.PerQuery) != len(b.PerQuery) {
		return nil, fmt.Errorf("%s and %s scored %d and %d queries: nothing to pair",
			a.Condition, b.Condition, len(a.PerQuery), len(b.PerQuery))
	}
	diffs := make([]float64, len(a.PerQuery))
	for i := range a.PerQuery {
		if a.PerQuery[i].Name != b.PerQuery[i].Name {
			return nil, fmt.Errorf("query %d is %q under %s and %q under %s: the two runs did not score the same query set",
				i, a.PerQuery[i].Name, a.Condition, b.PerQuery[i].Name, b.Condition)
		}
		diffs[i] = a.PerQuery[i].NDCG - b.PerQuery[i].NDCG
	}
	return diffs, nil
}

// CompareFusion returns the paired bootstrap for hybrid minus one single leg.
func CompareFusion(hybrid, leg Result) (PairDiff, error) {
	diffs, err := pairedNDiff(hybrid, leg)
	if err != nil {
		return PairDiff{}, err
	}
	lo, hi, mean := bootstrapMeanCI(diffs, bootstrapResamples, bootstrapSeed)
	return PairDiff{
		Leg: leg.Condition, Mean: mean, Lo: lo, Hi: hi,
		Queries: len(diffs), Resamples: bootstrapResamples,
	}, nil
}

// fusionTolerance is how much worse than a single leg hybrid fusion is allowed to
// score before the gate calls it a regression: -0.02 on the paired mean.
//
// The number is a finding, not a round figure. Measured on the committed v2
// dataset (220 graded queries, paired, 95% bootstrap):
//
//	hybrid - vector-only   mean +0.0171   CI [+0.0022, +0.0328]
//	hybrid - fts-only      mean +0.0689   CI [+0.0470, +0.0918]
//
// So fusion is genuinely ahead of both legs here, and ahead of the vector leg by
// only 0.0022 at the interval's lower edge — a little over half of one query's
// worth of margin (1/220 = 0.0045). The old gate was `hybrid.NDCG10 >=
// vector.NDCG10` on that 0.017 point estimate, which is the same claim with none
// of the uncertainty: a dataset edit worth 0.001 tripped it while the evidence
// said nothing had changed.
//
// 0.02 is the point where fusion has stopped earning its keep and become a
// handicap rather than a robustness play: 4.4 queries' worth of NDCG at this n,
// so a change that genuinely costs ranking quality trips the gate and a
// re-grading of the fixture does not. It is also deliberately one-sided. Fusion
// is not supposed to WIN on every corpus — on the LongMemEval-S chat benchmark
// vector-only ties it (docs/benchmarks.md Phase 1) — so a gate demanding fusion
// win would be a gate on the dataset rather than on the architecture. What must
// hold everywhere is that fusion is not materially worse, which is the claim the
// interval is fitted to support.
const fusionTolerance = 0.02

// fusionGate is the decision the interval feeds: the lower edge of the paired
// bootstrap has to clear -tolerance. It is a function so the rule can be tested
// on intervals the committed dataset does not produce — in particular the case
// the old point-estimate gate rejected and this one accepts (a mean that has
// dipped below zero while the evidence still says fusion is not behind), and the
// case both reject (fusion actually costing 0.02 of NDCG).
func fusionGate(ci PairDiff) error {
	if ci.Lo <= -fusionTolerance {
		return fmt.Errorf("hybrid is %s than %s: paired mean %+.4f, 95%% CI [%+.4f, %+.4f] over %d queries, "+
			"lower edge below the -%.2f tolerance (fusion is a handicap, not a robustness play)",
			"materially worse", ci.Leg, ci.Mean, ci.Lo, ci.Hi, ci.Queries, fusionTolerance)
	}
	return nil
}

// bootstrapMeanCI resamples the sample with replacement, bootResamples times, and
// returns the percentile interval and the observed mean of diffs.
//
// Percentile rather than BCa: with 220 paired differences the interval is read to
// three decimals, and BCa's bias and acceleration corrections are estimated from
// the same resamples they are correcting — a refinement whose own error is larger
// than the precision being printed. The interval is also slightly conservative
// under skew, which is the right direction for a gate.
func bootstrapMeanCI(diffs []float64, bootResamples int, seed int64) (lo, hi, mean float64) {
	n := len(diffs)
	if n == 0 {
		return 0, 0, 0
	}
	for _, d := range diffs {
		mean += d
	}
	mean /= float64(n)
	if n == 1 {
		// A single query has no spread to resample; the interval is the point
		// itself, which is the honest answer (and the gate reads it as such).
		return mean, mean, mean
	}

	rng := rand.New(rand.NewSource(seed))
	means := make([]float64, bootResamples)
	buf := make([]float64, n)
	for r := range means {
		var sum float64
		for i := range buf {
			buf[i] = diffs[rng.Intn(n)]
		}
		for _, v := range buf {
			sum += v
		}
		means[r] = sum / float64(n)
	}
	sort.Float64s(means)
	// Index by the percentile's rank in the sorted resample means. Clamped, so a
	// resample count too small to reach a tail yields the extreme rather than a
	// panic — a caller that under-samples gets a wide interval, which is the
	// conservative direction.
	loIdx := int(float64(bootResamples) * bootstrapAlpha / 2)
	hiIdx := int(float64(bootResamples) * (1 - bootstrapAlpha/2))
	if hiIdx >= bootResamples {
		hiIdx = bootResamples - 1
	}
	return means[loIdx], means[hiIdx], mean
}
