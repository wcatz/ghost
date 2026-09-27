package bench

import "testing"

// byCondition indexes results by their condition name.
func byCondition(results []Result) map[string]Result {
	m := make(map[string]Result, len(results))
	for _, r := range results {
		m[r.Condition] = r
	}
	return m
}

// TestBenchRegressionFloors guards retrieval quality on the committed dataset.
// Floors sit a little below the observed (deterministic) values so a genuine
// regression trips the build while normal dataset tweaks don't. Numbers are
// produced by TestBenchDatasetReport (-v).
func TestBenchRegressionFloors(t *testing.T) {
	r := byCondition(runTestdata(t))

	const wantQueries = 220
	for cond, res := range r {
		if res.Queries != wantQueries {
			t.Errorf("%s: scored %d queries, want %d", cond, res.Queries, wantQueries)
		}
	}

	floors := []struct {
		cond           string
		ndcg, recall10 float64
	}{
		// Observed on the v2 dataset (551 memories / 220 paraphrase-heavy
		// graded queries, committed embedding fixture): fts 0.749/0.697,
		// vector 0.800/0.764, hybrid 0.818/0.763 — the rows
		// docs/benchmarks.md publishes. Floors sit just below those, the
		// tightest being hybrid recall@10 with 0.013 of headroom.
		{CondFTS, 0.73, 0.67},
		{CondVector, 0.78, 0.75},
		{CondHybrid, 0.80, 0.75},
	}
	for _, f := range floors {
		res := r[f.cond]
		if res.NDCG10 < f.ndcg {
			t.Errorf("%s: NDCG@10 = %.3f, below floor %.2f", f.cond, res.NDCG10, f.ndcg)
		}
		if res.Recall10 < f.recall10 {
			t.Errorf("%s: recall@10 = %.3f, below floor %.2f", f.cond, res.Recall10, f.recall10)
		}
	}

	// The core architecture claim: hybrid fusion earns its keep over either
	// single leg. If this ever flips, the 70/30 weighting needs revisiting.
	if r[CondHybrid].NDCG10 < r[CondFTS].NDCG10 {
		t.Errorf("hybrid NDCG@10 %.3f must be >= fts-only %.3f", r[CondHybrid].NDCG10, r[CondFTS].NDCG10)
	}
	if r[CondHybrid].NDCG10 < r[CondVector].NDCG10 {
		t.Errorf("hybrid NDCG@10 %.3f must be >= vector-only %.3f", r[CondHybrid].NDCG10, r[CondVector].NDCG10)
	}
}
