package bench

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/linking"
	"github.com/wcatz/ghost/internal/memory"
)

// sweepFixture seeds the committed dataset into a fresh store and builds the
// link graph, mirroring what runBench does before a sweep.
func sweepFixture(t *testing.T) (*memory.Store, []Query) {
	t.Helper()
	ds, vecs := loadTestdataDataset(t)
	store := newBenchStore(t)
	ctx := context.Background()
	queries, err := Seed(ctx, store, ds, vecs)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	linking.NewWorker(store, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Hour, 0.70).SweepOnce(ctx)
	return store, queries
}

func TestSweep(t *testing.T) {
	store, queries := sweepFixture(t)
	ctx := context.Background()

	// A tiny grid containing the production default and graph-off.
	def := memory.DefaultSearchParams()
	off := def
	off.GraphWeight = 0
	points, err := Sweep(ctx, store, queries, []memory.SearchParams{def, off})
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

	// Cross-check both points against the ablation runner: default params ==
	// hybrid+graph, graph-off == hybrid. The ablation runner MUST start from a
	// link-free store (it builds the graph itself between conditions — a
	// pre-linked store would contaminate its plain-hybrid condition with the
	// graph bonus), so use runTestdata, not sweepFixture.
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
	if got, want := find(def).NDCG10, byCond[CondHybridGraph].NDCG10; got != want {
		t.Errorf("default sweep point NDCG %.6f != hybrid+graph ablation %.6f", got, want)
	}
	if got, want := find(off).NDCG10, byCond[CondHybrid].NDCG10; got != want {
		t.Errorf("graph-off sweep point NDCG %.6f != hybrid ablation %.6f", got, want)
	}
}

func TestSweepGrid(t *testing.T) {
	grid := SweepGrid()
	if len(grid) != 36 {
		t.Fatalf("grid size %d, want 36 (6 vec weights x 6 graph weights)", len(grid))
	}
	def := memory.DefaultSearchParams()
	foundDefault, foundGraphOff := false, false
	for _, p := range grid {
		if got := p.FTSWeight + p.VecWeight; got < 0.999 || got > 1.001 {
			t.Errorf("leg weights not normalized: fts=%.2f vec=%.2f", p.FTSWeight, p.VecWeight)
		}
		if p.RRFK != def.RRFK || p.GraphSeeds != def.GraphSeeds || p.GraphHops != def.GraphHops {
			t.Errorf("non-swept knobs must stay at defaults: %+v", p)
		}
		if p == def {
			foundDefault = true
		}
		if p.GraphWeight == 0 && p.VecWeight == def.VecWeight {
			foundGraphOff = true
		}
	}
	if !foundDefault {
		t.Error("grid must include the production default point")
	}
	if !foundGraphOff {
		t.Error("grid must include a graph-off point at the default leg weighting")
	}
}
