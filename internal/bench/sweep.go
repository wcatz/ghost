package bench

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/wcatz/ghost/internal/memory"
)

// SweepPoint is the outcome of evaluating one parameter combination over the
// query set with the hybrid searcher.
type SweepPoint struct {
	Params memory.SearchParams
	Result Result
}

// SweepGrid returns the default parameter grid: vector-leg weight (FTS weight
// is its complement, keeping the legs normalized to 1) crossed with the graph
// bonus weight, including 0 (graph off). RRF k and graph seeds/hops stay at
// production defaults — sweep one axis at a time.
func SweepGrid() []memory.SearchParams {
	vecWeights := []float64{0.3, 0.5, 0.6, 0.7, 0.8, 0.9}
	graphWeights := []float64{0, 0.02, 0.05, 0.10, 0.15, 0.30}
	grid := make([]memory.SearchParams, 0, len(vecWeights)*len(graphWeights))
	for _, vw := range vecWeights {
		for _, gw := range graphWeights {
			p := memory.DefaultSearchParams()
			p.VecWeight = vw
			// Round the complement so e.g. 1-0.7 is exactly 0.3 and the grid
			// contains a point == DefaultSearchParams (float identity matters
			// for the "current default" marker).
			p.FTSWeight = math.Round((1-vw)*100) / 100
			p.GraphWeight = gw
			grid = append(grid, p)
		}
	}
	return grid
}

// Sweep evaluates every parameter combination with the hybrid searcher over an
// already-seeded store whose link graph is already built (the graph pass reads
// links at query time, so one store serves every point). Results are sorted by
// NDCG@10 descending, ties broken by MRR@10 then recall@1.
func Sweep(ctx context.Context, store *memory.Store, queries []Query, grid []memory.SearchParams) ([]SweepPoint, error) {
	points := make([]SweepPoint, 0, len(grid))
	for _, p := range grid {
		p := p
		cond := fmt.Sprintf("vec=%.2f graph=%.2f", p.VecWeight, p.GraphWeight)
		res, err := runCondition(ctx, cond, queries, func(q Query) ([]string, error) {
			return idsFromMemories(store.SearchHybridParams(ctx, q.ProjectID, q.Text, q.Vector, scoreK, p))
		})
		if err != nil {
			return nil, err
		}
		points = append(points, SweepPoint{Params: p, Result: res})
	}
	sort.SliceStable(points, func(i, j int) bool {
		a, b := points[i].Result, points[j].Result
		if a.NDCG10 != b.NDCG10 {
			return a.NDCG10 > b.NDCG10
		}
		if a.MRR10 != b.MRR10 {
			return a.MRR10 > b.MRR10
		}
		return a.Recall1 > b.Recall1
	})
	return points, nil
}

// FormatSweep renders the sweep as an aligned table, best first, and marks the
// production-default row.
func FormatSweep(points []SweepPoint) string {
	var b bytes.Buffer
	def := memory.DefaultSearchParams()
	fmt.Fprintf(&b, "%-22s %7s %7s %8s %8s\n", "params", "R@1", "R@10", "MRR@10", "NDCG@10")
	for _, pt := range points {
		mark := ""
		if pt.Params == def {
			mark = "  <- current default"
		}
		fmt.Fprintf(&b, "%-22s %7.3f %7.3f %8.3f %8.3f%s\n",
			pt.Result.Condition, pt.Result.Recall1, pt.Result.Recall10, pt.Result.MRR10, pt.Result.NDCG10, mark)
	}
	if n := len(points); n > 0 {
		fmt.Fprintf(&b, "\n%d parameter combinations, sorted by NDCG@10; %d graded queries each.\n", n, points[0].Result.Queries)
	}
	return b.String()
}
