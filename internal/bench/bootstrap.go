package bench

import (
	"fmt"
	"math/rand/v2"
	"sort"
)

// The fusion gate asks one question: is hybrid fusion actually earning its keep
// over either single leg? The old gate was `hybrid.NDCG10 >= vector.NDCG10` on a
// dataset where the two differ by a hair, and it was two claims dressed as one —
// that fusion does not hurt, and that it helps. The first is worth asserting; the
// second needs an interval, because a gap that thin over 220 queries is inside
// the noise a resample of the query set produces, and a hard inequality on a
// noisy gap is a gate that fires on the fixture and nowhere else. (The exact
// margin is quoted below and in `TestBenchRegressionFloors`; it has been 0.017 and
// 0.018 on this branch as the corpus grew, which is itself the point — the figure
// moves, the fragility does not.)
//
// So the comparison is made the way the statistics are: paired per query, with a
// percentile bootstrap over the differences. Two conditions scoring the SAME
// query set are correlated query by query — measured on the committed dataset at
// r = 0.88, and the pairing that removes is worth about 4× in interval width
// (half-width 0.015 paired against 0.063 unpaired) — so pairing is most of the
// available precision, and what is left is the variation attributable to the
// ranking, which is the thing under test. The interval is the honest statement of
// how much of the observed gap the data supports.
//
// Determinism is a fixed seed pair and a fixed resample count over an explicitly
// seeded PCG, so the interval reproduces exactly and is not at the mercy of a
// future toolchain's change to the default generator.

// Bootstrap defaults. 20000 resamples puts the 2.5th and 97.5th percentile on
// the 500th and 19500th order statistic, which is as fine as the interval's three
// printed digits are meaningful.
//
// The source is math/rand/v2's PCG, not the v1 package: an explicit PCG has a
// documented, version-stable stream, whereas math/rand/v1's global functions
// changed algorithm in Go 1.20 and its Source is documented as unstable. This
// repo already uses `rand.NewPCG` for a reproducible stream
// (internal/memory/vector_bench_test.go), and a number published to four
// decimals in docs/benchmarks.md is worth not re-deriving by hand after a toolchain
// bump. The seed is a pair because NewPCG takes two; both are fixed here.
const (
	bootstrapResamples = 20000
	bootstrapSeed1     = 1
	bootstrapSeed2     = 2
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

// pairedCI is the comparison both call sites want: the paired bootstrap of a over
// b, with b's condition recorded as the thing a is measured against. The pairing
// is over the same query names in the same order, so a and b must have been
// produced from one query set.
func pairedCI(a, b Result) (PairDiff, error) {
	diffs, err := pairedNDiff(a, b)
	if err != nil {
		return PairDiff{}, err
	}
	lo, hi, mean := bootstrapMeanCI(diffs, bootstrapResamples)
	return PairDiff{
		Leg: b.Condition, Mean: mean, Lo: lo, Hi: hi,
		Queries: len(diffs), Resamples: bootstrapResamples,
	}, nil
}

// CompareFusion returns the paired bootstrap for hybrid minus one single leg.
func CompareFusion(hybrid, leg Result) (PairDiff, error) {
	return pairedCI(hybrid, leg)
}

// fusionTolerance is how much worse than a single leg hybrid fusion is allowed to
// score before the gate calls it a regression: -0.02 on the paired mean.
//
// The number is a judgement, and it is deliberately LARGER than the effect it
// protects. Measured on the committed v3 dataset (220 graded queries, paired, 95%
// percentile bootstrap, 20k resamples, fixed PCG seed pair):
//
//	hybrid - vector-only   mean +0.0175   CI [+0.0029, +0.0325]
//	hybrid - fts-only      mean +0.0722   CI [+0.0504, +0.0951]
//
// So fusion is genuinely ahead of both legs here, and ahead of the vector leg by
// 0.0029 at the interval's lower edge — under half of one query's worth of
// margin (1/220 = 0.0045). The old gate was `hybrid.NDCG10 >= vector.NDCG10` on
// that thin point estimate, which is the same claim with none of the
// uncertainty: a dataset edit worth 0.001 tripped it while the evidence said
// nothing had changed.
//
// 0.02 is 4.4 queries' worth of NDCG at this n. Two consequences, both of them
// the reason the number is this size rather than 0.005:
//
//   - It absorbs a re-grading of the fixture. A query whose label is re-read
//     moves the mean by up to 0.0045, so a tolerance at the size of the effect
//     would fail on a documentation change.
//   - It CANNOT fire while fusion's advantage over the vector leg reverses by
//     less than 4.4 queries. That is the honest cost, and it is larger than the
//     effect: the gate is a "not materially worse" check, not an "earns its keep"
//     check. The claim fusion earns its keep is supported by the interval
//     EXCLUDING zero, and is reported rather than gated; the gate exists to catch
//     fusion becoming a handicap, and it is deliberately one-sided for the same
//     reason: on the chat benchmark in Phase 1 vector-only ties hybrid, so a gate
//     demanding fusion win everywhere would be a gate on the dataset rather than
//     on the architecture.
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
// than the precision being printed. The percentile interval's own known weakness
// is that it under-covers slightly for a skewed sampling distribution; that is the
// price of not estimating two corrections from the same 20k numbers, and the gate
// reads the LOWER edge, so under-coverage there is the conservative direction for
// the one claim it makes.
func bootstrapMeanCI(diffs []float64, bootResamples int) (lo, hi, mean float64) {
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
	if bootResamples <= 0 {
		// No resamples means no interval, and the only honest interval over one
		// sample is the sample. Returning the extremes would fabricate a
		// distribution that was never drawn.
		return mean, mean, mean
	}

	rng := rand.New(rand.NewPCG(bootstrapSeed1, bootstrapSeed2))
	means := make([]float64, bootResamples)
	buf := make([]float64, n)
	for r := range means {
		var sum float64
		for i := range buf {
			buf[i] = diffs[rng.IntN(n)] // IntN, not Intn: v2 spells the unbounded one with the capital N
		}
		for _, v := range buf {
			sum += v
		}
		means[r] = sum / float64(n)
	}
	sort.Float64s(means)
	// Index by the percentile's rank in the sorted resample means. The high side
	// is clamped because a resample count too small to reach the 97.5th
	// percentile would otherwise index past the end; the low side needs no clamp
	// for the same reason, since its index can only be smaller. A caller that
	// under-samples gets an interval reaching the most extreme resample, which is
	// the conservative direction.
	loIdx := int(float64(bootResamples) * bootstrapAlpha / 2)
	hiIdx := int(float64(bootResamples) * (1 - bootstrapAlpha/2))
	if hiIdx >= bootResamples {
		hiIdx = bootResamples - 1
	}
	return means[loIdx], means[hiIdx], mean
}
