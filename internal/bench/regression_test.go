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
	// Parallel: this test seeds and searches the immutable headline corpus and only
	// reads it, and at 60-130s under -race it is one of the five that decide whether
	// this package fits Go's 600s per-binary default — a budget it had already
	// spent down to ~10s when #677 added a sixth corpus-wide test, not a budget that
	// rows cost. There is no shared state to order against — the
	// package-level values are embedded bytes and one constant floor slice, and no
	// bench test sets an env var or the default logger — so the only thing running
	// these together buys is the overlap. Measured, in the commit that added this.
	t.Parallel()
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
		// Observed on the v3 dataset (549 memories / 220 paraphrase-heavy
		// graded queries, committed embedding fixture): fts 0.759/0.702,
		// vector 0.814/0.766, hybrid 0.831/0.765 — the rows
		// docs/benchmarks.md publishes. Floors sit just below those, the
		// tightest being hybrid recall@10 with 0.013 of headroom.
		//
		// #965 re-floored these against a corrected LABEL SET, not a changed
		// ranking: fifteen queries gained a second row at the gain their
		// content earns and two byte-identical corpus rows were dropped, so
		// every floor moved with the measurement it guards rather than being
		// left behind it. Each floor keeps the margin its own row already
		// carried (observed minus floored: 0.019 / 0.027 / 0.020 / 0.014 /
		// 0.018 / 0.013), so a row's headroom is a property of the row and
		// not of when it was written.
		{CondFTS, 0.74, 0.67},
		{CondVector, 0.79, 0.75},
		{CondHybrid, 0.81, 0.75},
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
	// single leg. It is asserted as a PAIRED interval rather than a comparison of
	// two means: the two conditions score the same 220 queries, so pairing
	// removes most of the variance and leaves a difference the data can speak
	// about. The claim is that fusion is not materially worse (see
	// fusionTolerance for the number and the measurement behind it), not that it
	// wins everywhere — on the chat benchmark in Phase 1 it ties.
	//
	// The old gate was `hybrid.NDCG10 >= vector.NDCG10` on a thin point
	// estimate, which fails on a 0.001 dataset edit while the interval still
	// excludes zero. The intervals are logged because they are the result: on the
	// committed v3 dataset hybrid beats vector by +0.0175 [+0.0029, +0.0325] and
	// fts by +0.0722 [+0.0504, +0.0951].
	for _, leg := range []string{CondVector, CondFTS} {
		ci, err := CompareFusion(r[CondHybrid], r[leg])
		if err != nil {
			t.Fatalf("hybrid vs %s: %v", leg, err)
		}
		t.Logf("hybrid - %-12s paired mean %+.4f, 95%% CI [%+.4f, %+.4f] over %d queries",
			ci.Leg, ci.Mean, ci.Lo, ci.Hi, ci.Queries)
		if err := fusionGate(ci); err != nil {
			t.Errorf("%v", err)
		}
	}
}
