package bench

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
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

// runTestdata is the committed dataset seeded into a fresh store and scored under
// all three ablations. The run is a pure function of the committed fixture
// (corpusID and corpusStamp make the store itself one, #708) and the three tests
// that call it only READ the result, so it is computed ONCE per package and
// shared, on the model of auditRun: it is ~47s of -race CPU (a seed and three
// 220-query passes) and it was being paid three times over.
//
// The store is opened and closed inside the Once, on a background context, and
// not through newBenchStoreWithDB(t): the first caller's t.Cleanup would then own
// a store the other callers still read, and a t.Fatalf inside the Once would end
// the first caller's goroutine with the Once already marked done and no result.
// The error is kept instead and every caller reports it. TestSweep's cross-check
// still compares two independently seeded stores, because sweepFixture seeds its
// own, and two seeds of one corpus stay the subject of
// TestSweepReproducesAcrossTwoSeedsInOneProcess.
func runTestdata(t *testing.T) []Result {
	t.Helper()
	testdataOnce.Do(func() { testdataShared, testdataSharedErr = computeTestdata() })
	if testdataSharedErr != nil {
		t.Fatalf("%v", testdataSharedErr)
	}
	// A copy of the outer slice, so a caller that reorders its own view cannot
	// reorder another's; the Results are read-only to every caller.
	return append([]Result(nil), testdataShared...)
}

var (
	testdataOnce      sync.Once
	testdataShared    []Result
	testdataSharedErr error
)

func computeTestdata() ([]Result, error) {
	ds, err := LoadDatasetFiles("testdata", "bench")
	if err != nil {
		return nil, fmt.Errorf("load dataset: %w", err)
	}
	f, err := os.Open("testdata/embeddings.json")
	if err != nil {
		return nil, fmt.Errorf("open embeddings: %w", err)
	}
	defer f.Close() //nolint:errcheck
	vecs, err := LoadVectors(f)
	if err != nil {
		return nil, fmt.Errorf("load vectors: %w", err)
	}
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	store := memory.NewStore(db, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	defer store.Close() //nolint:errcheck
	ctx := context.Background()
	queries, err := Seed(ctx, store, db, ds, vecs)
	if err != nil {
		return nil, fmt.Errorf("seed: %w", err)
	}
	results, err := Run(ctx, store, queries)
	if err != nil {
		return nil, fmt.Errorf("run: %w", err)
	}
	return results, nil
}

// TestBenchDatasetReport runs the three ablations over the committed dataset and
// logs the metric table. It is the human-readable report; run with -v.
func TestBenchDatasetReport(t *testing.T) {
	// Parallel: this test seeds and searches the immutable headline corpus and only
	// reads it, and at 60-130s under -race it is one of the corpus-wide tests that decide whether
	// this package fits Go's 600s per-binary default — a budget it had already
	// spent down to ~10s when #677 added a sixth corpus-wide test, not a budget that
	// rows cost. There is no shared state to order against — the
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
