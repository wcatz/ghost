package bench

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// TestFormatFusionGapsPrintsTheHeadlineInterval: the fusion margin is the
// headline number in docs/benchmarks.md and README.md, and it used to exist only
// as a t.Logf in the regression test — a published interval no command prints,
// which is the defect #561 is about. `ghost bench` has to print it, from the same
// function the gate reads, so the doc and the build cannot disagree.
//
// Every field is checked rather than the string: a row can carry a plausible mean
// and the wrong interval, and the doc quotes both.
func TestFormatFusionGapsPrintsTheHeadlineInterval(t *testing.T) {
	// Four queries, the fused condition ahead on two of them and level on two, so
	// the paired mean is positive and the interval straddles zero — the shape the
	// committed dataset actually produces, and the one the doc's claim rests on.
	per := func(name string, vals ...float64) []QueryScore {
		out := make([]QueryScore, len(vals))
		for i, v := range vals {
			out[i] = QueryScore{Name: name + string(rune('a'+i)), NDCG: v}
		}
		return out
	}
	results := []Result{
		{Condition: CondHybrid, NDCG10: 0.8, Queries: 4, PerQuery: per("q", 1.0, 1.0, 0.5, 0.5)},
		{Condition: CondVector, NDCG10: 0.7, Queries: 4, PerQuery: per("q", 0.5, 0.5, 0.5, 0.5)},
		{Condition: CondFTS, NDCG10: 0.6, Queries: 4, PerQuery: per("q", 0.2, 0.2, 0.2, 0.2)},
	}
	out := FormatFusionGaps(results)

	for _, want := range []string{"fused - " + CondVector, "fused - " + CondFTS, "paired per query", "95% percentile bootstrap"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not mention %q:\n%s", want, out)
		}
	}
	// Both legs get a row; a leg that happened to tie is still a row, because
	// "not separable" is a finding and a missing row is not.
	if n := strings.Count(out, "fused - "); n != 2 {
		t.Errorf("%d comparison rows, want 2:\n%s", n, out)
	}

	// The vector row's numbers, recomputed from the same Results rather than
	// matched as text: (0.5, 0.5, 0, 0) has mean +0.25.
	hybrid, _ := resultFor(results, CondHybrid)
	vector, _ := resultFor(results, CondVector)
	want, err := CompareFusion(hybrid, vector)
	if err != nil {
		t.Fatalf("CompareFusion: %v", err)
	}
	if math.Abs(want.Mean-0.25) > 1e-12 {
		t.Fatalf("fixture mean = %+.6f, want +0.25; the test is not measuring what it thinks", want.Mean)
	}
	if !strings.Contains(out, sprintf4(want.Mean, want.Lo, want.Hi)) {
		t.Errorf("the printed report does not carry the recomputed interval %s:\n%s", sprintf4(want.Mean, want.Lo, want.Hi), out)
	}
	// The verdict has to follow the interval, not the sign of the mean: a positive
	// mean with an interval reaching zero is NOT "ahead of the leg".
	if want.Lo <= 0 && !strings.Contains(out, "not separable from the leg") {
		t.Errorf("an interval containing zero is called something other than not separable:\n%s", out)
	}
	// And the fused-vs-FTS row here IS separable (constant +0.4), so both
	// verdicts have to be reachable from the same formatter.
	if !strings.Contains(out, "ahead of the leg") {
		t.Errorf("a constant positive difference was not called ahead of the leg:\n%s", out)
	}
}

// sprintf4 renders an interval the way the report does, so the test compares the
// printed numbers against a recomputation rather than against another copy of the
// format string.
func sprintf4(mean, lo, hi float64) string {
	return fmt.Sprintf("%+9.4f %+9.4f %+9.4f", mean, lo, hi)
}
