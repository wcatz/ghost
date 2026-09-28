package bench

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"reflect"
	"sort"

	"github.com/wcatz/ghost/internal/memory"
)

// SweepPoint is the outcome of evaluating one parameter combination over the
// query set with the hybrid searcher.
type SweepPoint struct {
	Params memory.SearchParams
	Result Result

	// VsDefault is the paired per-query NDCG@10 difference of this grid point
	// against the shipped default, with a percentile bootstrap interval over it.
	// It is nil for the default point, which has nothing to be compared against.
	//
	// The sweep sorts by NDCG@10, and a sorted table of point estimates is
	// precisely the claim #561 is about: a 0.004 gap over 220 queries is not a
	// finding, and reading the order as a ranking is how the grid came to be
	// described as "0.3-0.7 flat" when two of its points are separable and the
	// rest are not. The interval is what makes the row readable, because it
	// separates a point that is genuinely better from one that is tied.
	//
	// The sign is point minus default, so a positive mean means this grid point
	// scored better than what ships.
	VsDefault *PairDiff
}

// SweepGrid returns the default parameter grid: vector-leg weight (FTS weight
// is its complement, keeping the legs normalized to 1). RRF k stays at the
// production default — sweep one axis at a time.
func SweepGrid() []memory.SearchParams {
	vecWeights := []float64{0.3, 0.5, 0.6, 0.7, 0.8, 0.9}
	grid := make([]memory.SearchParams, 0, len(vecWeights))
	for _, vw := range vecWeights {
		p := memory.DefaultSearchParams()
		p.VecWeight = vw
		// Round the complement so e.g. 1-0.7 is exactly 0.3 and the grid
		// contains a point equal to DefaultSearchParams.
		p.FTSWeight = math.Round((1-vw)*100) / 100
		grid = append(grid, p)
	}
	return grid
}

// Sweep evaluates every parameter combination with the hybrid searcher over an
// already-seeded store (one store serves every point). Results are sorted by
// NDCG@10 descending, ties broken by MRR@10 then recall@1.
//
// Every point also carries a paired interval against the shipped default
// (SweepPoint.VsDefault), because the sort order is a point-estimate order and a
// point estimate is not a finding. The intervals are computed after the sort, so
// the comparison is always against the default rather than against whichever
// point happens to be printed first.
//
// A grid with no point equal to the shipped default yields no intervals at all
// rather than intervals against an arbitrary member: there is nothing here that
// says which of these points is the one in production, and a comparison made
// against a guess would be the kind of claim this column exists to remove. The
// rows are then marked as un-compared, so the absence is visible.
//
// The sweep is a relevance search, so it scores the graded set only: a
// no-answer query in the set would be measured for false positives
// (Result.NoAnswer) and read nowhere here, and the false-positive rate is
// reported per condition by FormatResults rather than per grid point.
func Sweep(ctx context.Context, store *memory.Store, queries []Query, grid []memory.SearchParams) ([]SweepPoint, error) {
	points := make([]SweepPoint, 0, len(grid))
	for _, p := range grid {
		cond := fmt.Sprintf("vec=%.2f", p.VecWeight)
		res, err := runCondition(ctx, store, cond, queries, func(q Query) ([]string, error) {
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
	if err := pairAgainstDefault(points); err != nil {
		return nil, err
	}
	return points, nil
}

// isDefaultParams reports whether p is the shipped default. DeepEqual, not ==:
// SearchParams carries a Scope map, which makes the struct non-comparable. The
// sweep never sets Scope, so this only has to stay correct for the
// default-versus-default case.
func isDefaultParams(p memory.SearchParams) bool {
	return reflect.DeepEqual(p, memory.DefaultSearchParams())
}

// pairAgainstDefault fills in VsDefault on every point that is not the shipped
// default, pairing it against the default point's per-query NDCG. A grid carrying
// the default twice would compare the second against the first, so the FIRST such
// point is the reference and a later one is measured against it. That is the right
// reading of a grid that lists a point twice by mistake, and
// TestSweepPairsEveryOtherPointAgainstTheDefaultAndPrintsIt fails such a grid
// rather than letting the mistake reach a report.
func pairAgainstDefault(points []SweepPoint) error {
	def := -1
	for i := range points {
		if isDefaultParams(points[i].Params) {
			def = i
			break
		}
	}
	if def < 0 {
		return nil
	}
	for i := range points {
		if i == def {
			continue
		}
		ci, err := pairedCI(points[i].Result, points[def].Result)
		if err != nil {
			return err
		}
		points[i].VsDefault = &ci
	}
	return nil
}

// sweepHeader is the table's column line. It is a function so the report and the
// docs/benchmarks.md table that quotes it are compared against one string, rather
// than against a copy in a test that drifts on the first column rename.
func sweepHeader() string {
	return fmt.Sprintf("%-22s %7s %7s %8s %8s  %s", "params", "R@1", "R@10", "MRR@10", "NDCG@10", "vs default (paired 95%)")
}

// FormatSweep renders the sweep as an aligned table, best first, marks the
// production-default row, and prints each point's paired interval against the
// default. The default row's own column says so rather than printing a zero
// interval, which would read as "indistinguishable from the default" rather than
// as the absence of a comparison.
func FormatSweep(points []SweepPoint) string {
	var b bytes.Buffer
	fmt.Fprintln(&b, sweepHeader())
	for _, pt := range points {
		mark, ci := "", "no default in grid"
		switch {
		case isDefaultParams(pt.Params):
			mark = "  <- current default"
			ci = "this is the default"
		case pt.VsDefault != nil:
			ci = fmt.Sprintf("%+.4f [%+.4f, %+.4f]", pt.VsDefault.Mean, pt.VsDefault.Lo, pt.VsDefault.Hi)
		}
		fmt.Fprintf(&b, "%-22s %7.3f %7.3f %8.3f %8.3f  %s%s\n",
			pt.Result.Condition, pt.Result.Recall1, pt.Result.Recall10, pt.Result.MRR10, pt.Result.NDCG10, ci, mark)
	}
	if n := len(points); n > 0 {
		fmt.Fprintf(&b, "\n%d parameter combinations, sorted by NDCG@10; %d graded queries each.\n", n, points[0].Result.Queries)
		fmt.Fprintf(&b, "The sort is by point estimate. The interval column is the paired per-query NDCG@10 difference against the shipped default;\n")
		fmt.Fprintf(&b, "an interval containing 0.0 means the point is not separable from the default, whatever its position in the sort.\n")
		fmt.Fprintf(&b, "The store breaks tied fused scores by memory id and the benchmark seeds every id from randomblob, so a point whose two\n")
		fmt.Fprintf(&b, "legs are weighted equally re-draws that tie-break on every run: vec=0.50 here (#708), the other five reproduce exactly.\n")
		fmt.Fprintf(&b, "Read that row's interval as a shape rather than as four decimals.\n")
	}
	return b.String()
}
