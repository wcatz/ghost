package bench

import (
	"fmt"
	"math/rand/v2"
	"sort"
)

const (
	bootstrapResamples = 20000
	bootstrapSeed1     = 1
	bootstrapSeed2     = 2
	bootstrapAlpha     = 0.05
)

// PairDiff is the per-query NDCG@10 difference between two conditions.
type PairDiff struct {
	Leg       string
	Mean      float64
	Lo, Hi    float64
	Queries   int
	Resamples int
}

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

const fusionTolerance = 0.02

func fusionGate(ci PairDiff) error {
	if ci.Lo <= -fusionTolerance {
		return fmt.Errorf("hybrid is %s than %s: paired mean %+.4f, 95%% CI [%+.4f, %+.4f] over %d queries, "+
			"lower edge below the -%.2f tolerance (fusion is a handicap, not a robustness play)",
			"materially worse", ci.Leg, ci.Mean, ci.Lo, ci.Hi, ci.Queries, fusionTolerance)
	}
	return nil
}

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
		return mean, mean, mean
	}
	if bootResamples <= 0 {
		return mean, mean, mean
	}

	rng := rand.New(rand.NewPCG(bootstrapSeed1, bootstrapSeed2))
	means := make([]float64, bootResamples)
	buf := make([]float64, n)
	for r := range means {
		var sum float64
		for i := range buf {
			buf[i] = diffs[rng.IntN(n)]
		}
		for _, v := range buf {
			sum += v
		}
		means[r] = sum / float64(n)
	}
	sort.Float64s(means)
	loIdx := int(float64(bootResamples) * bootstrapAlpha / 2)
	hiIdx := int(float64(bootResamples) * (1 - bootstrapAlpha/2))
	if hiIdx >= bootResamples {
		hiIdx = bootResamples - 1
	}
	return means[loIdx], means[hiIdx], mean
}
