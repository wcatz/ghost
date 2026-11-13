package bench

import (
	"context"
	"reflect"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// sweepFixture seeds the committed dataset into a fresh store, mirroring what
// runBench does before a sweep.
func sweepFixture(t *testing.T) (*memory.Store, []Query) {
	t.Helper()
	ds, vecs := loadTestdataDataset(t)
	store, db := newBenchStoreWithDB(t)
	ctx := context.Background()
	queries, err := Seed(ctx, store, db, ds, vecs)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return store, queries
}

func TestSweep(t *testing.T) {
	// Parallel: this test seeds and searches the immutable headline corpus and only
	// reads it, and at 60-130s under -race it is one of the corpus-wide tests that decide whether
	// this package fits Go's 600s per-binary default — a budget it had already
	// spent down to ~10s when #677 added a sixth corpus-wide test, not a budget that
	// rows cost. There is no shared state to order against — the
	// package-level values are embedded bytes and one constant floor slice, and no
	// bench test sets an env var or the default logger — so the only thing running
	// these together buys is the overlap. Measured, in the commit that added this.
	t.Parallel()
	store, queries := sweepFixture(t)
	ctx := context.Background()

	// Two points: the production default and an off-default leg weighting.
	def := memory.DefaultSearchParams()
	alt := def
	alt.VecWeight = 0.9
	alt.FTSWeight = 0.1
	points, err := Sweep(ctx, store, queries, []memory.SearchParams{def, alt})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(points) != 2 {
		t.Fatalf("got %d points, want 2", len(points))
	}

	// Sorted by NDCG@10 descending.
	if points[0].Result.NDCG10 < points[1].Result.NDCG10 {
		t.Errorf("points not sorted by NDCG: %.3f then %.3f", points[0].Result.NDCG10, points[1].Result.NDCG10)
	}

	// Cross-check the default point against the ablation runner: default
	// params == the hybrid ablation.
	byCond := byCondition(runTestdata(t))
	find := func(p memory.SearchParams) Result {
		for _, pt := range points {
			// DeepEqual, not ==: SearchParams carries a Scope map and is
			// therefore not comparable with ==.
			if reflect.DeepEqual(pt.Params, p) {
				return pt.Result
			}
		}
		t.Fatalf("sweep point not found for %+v", p)
		return Result{}
	}
	if got, want := find(def).NDCG10, byCond[CondHybrid].NDCG10; got != want {
		t.Errorf("default sweep point NDCG %.6f != hybrid ablation %.6f", got, want)
	}
}
