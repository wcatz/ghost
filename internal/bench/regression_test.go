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

	const wantQueries = 14
	for cond, res := range r {
		if res.Queries != wantQueries {
			t.Errorf("%s: scored %d queries, want %d", cond, res.Queries, wantQueries)
		}
	}

	floors := []struct {
		cond           string
		ndcg, recall10 float64
	}{
		{CondFTS, 0.92, 0.95},
		{CondVector, 0.90, 0.90},
		{CondHybrid, 0.95, 0.95},
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

	// Tracked, not asserted: the hybrid+graph ablation opts into the candidate
	// graph weight (the former default — production now ships GraphWeight 0
	// after the sweep measured the bonus degrading ranking monotonically).
	// The gap is logged so redesign progress is visible; a graph redesign
	// ships when this inverts, i.e. it beats plain hybrid here and in the
	// sweep (see docs/benchmarks.md).
	if g, h := r[CondHybridGraph].NDCG10, r[CondHybrid].NDCG10; g < h {
		t.Logf("note: hybrid+graph (candidate weight %.2f) NDCG@10 %.3f < hybrid %.3f — graph bonus disabled in production defaults (see docs/benchmarks.md)", candidateGraphWeight, g, h)
	}
}
