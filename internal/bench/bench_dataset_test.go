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
// four ablations against it.
func runTestdata(t *testing.T) []Result {
	t.Helper()
	ds, vecs := loadTestdataDataset(t)
	store := newBenchStore(t)
	ctx := context.Background()
	queries, err := Seed(ctx, store, ds, vecs)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	results, err := Run(ctx, store, queries, 0.70)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return results
}

// TestBenchDatasetReport runs the four ablations over the committed dataset and
// logs the metric table. It is the human-readable report; run with -v.
func TestBenchDatasetReport(t *testing.T) {
	results := runTestdata(t)
	t.Logf("%-14s %7s %7s %7s %7s %7s  (n=%d)", "condition", "R@1", "R@5", "R@10", "MRR@10", "NDCG@10", results[0].Queries)
	for _, r := range results {
		t.Logf("%-14s %7.3f %7.3f %7.3f %7.3f %7.3f", r.Condition, r.Recall1, r.Recall5, r.Recall10, r.MRR10, r.NDCG10)
	}
}
