package bench

import (
	"context"
	"os"
	"testing"
)

// loadTestdataDataset loads the committed benchmark dataset and its embedding
// fixture from testdata/. Shared by the reporting and regression tests.
func loadTestdataDataset(t *testing.T) (Dataset, Vectors) {
	t.Helper()
	ds, err := LoadDatasetFiles("testdata", "bench")
	if err != nil {
		t.Fatalf("load dataset: %v", err)
	}
	f, err := os.Open("testdata/embeddings.json")
	if err != nil {
		t.Fatalf("open embeddings: %v", err)
	}
	defer f.Close() //nolint:errcheck
	vecs, err := LoadVectors(f)
	if err != nil {
		t.Fatalf("load vectors: %v", err)
	}
	return ds, vecs
}

// runTestdata seeds a fresh store from the committed dataset and evaluates all
// three ablations against it.
func runTestdata(t *testing.T) []Result {
	t.Helper()
	ds, vecs := loadTestdataDataset(t)
	store, db := newBenchStoreWithDB(t)
	ctx := context.Background()
	queries, err := Seed(ctx, store, db, ds, vecs)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	results, err := Run(ctx, store, queries)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return results
}

// TestBenchDatasetReport runs the three ablations over the committed dataset and
// logs the metric table. It is the human-readable report; run with -v.
func TestBenchDatasetReport(t *testing.T) {
	// Parallel: this test seeds and searches the immutable headline corpus and only
	// reads it, and at 60-130s under -race it is one of the five that decide whether
	// this package fits Go's 600s per-binary default. The corpus grew by four rows in
	// #677 and took it over. There is no shared state to order against — the
	// package-level values are embedded bytes and one constant floor slice, and no
	// bench test sets an env var or the default logger — so the only thing running
	// these together buys is the overlap. Measured, in the commit that added this.
	t.Parallel()
	results := runTestdata(t)
	t.Logf("%-14s %7s %7s %7s %7s %7s  (n=%d)", "condition", "R@1", "R@5", "R@10", "MRR@10", "NDCG@10", results[0].Queries)
	for _, r := range results {
		t.Logf("%-14s %7.3f %7.3f %7.3f %7.3f %7.3f", r.Condition, r.Recall1, r.Recall5, r.Recall10, r.MRR10, r.NDCG10)
	}
}
