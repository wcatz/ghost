package bench

import (
	"context"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// sweepFixture seeds the committed dataset into a fresh store, mirroring what
// runBench does before a sweep.
func sweepFixture(t *testing.T) (*memory.Store, []Query) {
	t.Helper()
	ds, vecs := loadTestdataDataset(t)
	store := newBenchStore(t)
	ctx := context.Background()
	queries, err := Seed(ctx, store, ds, vecs)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return store, queries
}

func TestSweep(t *testing.T) {
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
			if pt.Params == p {
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

func TestSweepGrid(t *testing.T) {
	grid := SweepGrid()
	if len(grid) != 6 {
		t.Fatalf("grid size %d, want 6 (6 vec weights)", len(grid))
	}
	def := memory.DefaultSearchParams()
	foundDefault := false
	for _, p := range grid {
		if got := p.FTSWeight + p.VecWeight; got < 0.999 || got > 1.001 {
			t.Errorf("leg weights not normalized: fts=%.2f vec=%.2f", p.FTSWeight, p.VecWeight)
		}
		if p.RRFK != def.RRFK {
			t.Errorf("non-swept knobs must stay at defaults: %+v", p)
		}
		if p == def {
			foundDefault = true
		}
	}
	if !foundDefault {
		t.Error("grid must include the production default point")
	}
}
