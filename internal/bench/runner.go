package bench

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/wcatz/ghost/internal/linking"
	"github.com/wcatz/ghost/internal/memory"
)

// scoreK is the retrieval depth: results are scored at recall@1/5/10, so the
// search must return at least this many candidates.
const scoreK = 10

// Query is one benchmark question: its text, an optional precomputed embedding
// (required for the vector and hybrid conditions), the project to search, and
// the graded relevance of memory IDs.
type Query struct {
	Name      string
	ProjectID string
	Text      string
	Vector    []float32
	Rel       Relevance
}

// Result holds aggregate metrics for one search condition over a query set.
type Result struct {
	Condition string
	Queries   int // queries with at least one relevant item (the scored set)
	Recall1   float64
	Recall5   float64
	Recall10  float64
	MRR10     float64
	NDCG10    float64
}

// Condition names, stable for reporting.
const (
	CondFTS         = "fts-only"
	CondVector      = "vector-only"
	CondHybrid      = "hybrid"
	CondHybridGraph = "hybrid+graph"
)

// candidateGraphWeight is the graph-bonus weight the hybrid+graph ablation
// opts into. It is the former production default, kept under measurement
// after the sweep showed it degrades ranking; production defaults now ship
// with GraphWeight 0.
const candidateGraphWeight = 0.15

// Run evaluates all four ablations over the seeded store and query set. The
// graph condition is run last: it first builds the memory-link graph (the same
// linking worker production uses, at the given cosine threshold), which
// SearchHybrid then folds in as an additive bonus. Running it last keeps the
// plain hybrid condition free of link effects on the same store.
func Run(ctx context.Context, store *memory.Store, queries []Query, linkThreshold float32) ([]Result, error) {
	fts, err := runCondition(ctx, CondFTS, queries, func(q Query) ([]string, error) {
		return idsFromMemories(store.SearchFTS(ctx, q.ProjectID, q.Text, scoreK))
	})
	if err != nil {
		return nil, err
	}
	vec, err := runCondition(ctx, CondVector, queries, func(q Query) ([]string, error) {
		return idsFromScored(store.SearchVector(ctx, q.ProjectID, q.Vector, scoreK))
	})
	if err != nil {
		return nil, err
	}
	hybrid, err := runCondition(ctx, CondHybrid, queries, func(q Query) ([]string, error) {
		return idsFromMemories(store.SearchHybrid(ctx, q.ProjectID, q.Text, q.Vector, scoreK))
	})
	if err != nil {
		return nil, err
	}

	// Build the link graph, then re-run hybrid with the CANDIDATE graph weight
	// so the graph-expansion bonus participates. Production defaults ship with
	// GraphWeight 0 (the sweep showed the bonus degrades ranking — see
	// docs/benchmarks.md), so this ablation deliberately opts in: it keeps
	// measuring the signal a redesign must improve. SweepOnce does a single
	// pass; the interval is irrelevant.
	linking.NewWorker(store, slog.New(slog.NewTextHandler(discard{}, nil)), time.Hour, linkThreshold).SweepOnce(ctx)
	pGraph := memory.DefaultSearchParams()
	pGraph.GraphWeight = candidateGraphWeight
	hybridGraph, err := runCondition(ctx, CondHybridGraph, queries, func(q Query) ([]string, error) {
		return idsFromMemories(store.SearchHybridParams(ctx, q.ProjectID, q.Text, q.Vector, scoreK, pGraph))
	})
	if err != nil {
		return nil, err
	}

	return []Result{fts, vec, hybrid, hybridGraph}, nil
}

// rankFn returns the ranked memory IDs for one query under a condition.
type rankFn func(q Query) ([]string, error)

func runCondition(ctx context.Context, name string, queries []Query, rank rankFn) (Result, error) {
	res := Result{Condition: name}
	var sumR1, sumR5, sumR10, sumMRR, sumNDCG float64
	for _, q := range queries {
		if q.Rel.relevantCount() == 0 {
			continue // a query with no relevant items is undefined for these ratios
		}
		ranked, err := rank(q)
		if err != nil {
			return Result{}, fmt.Errorf("%s: query %q: %w", name, q.Name, err)
		}
		res.Queries++
		sumR1 += RecallAtK(ranked, q.Rel, 1)
		sumR5 += RecallAtK(ranked, q.Rel, 5)
		sumR10 += RecallAtK(ranked, q.Rel, 10)
		sumMRR += ReciprocalRankAtK(ranked, q.Rel, 10)
		sumNDCG += NDCGAtK(ranked, q.Rel, 10)
	}
	if res.Queries > 0 {
		n := float64(res.Queries)
		res.Recall1 = sumR1 / n
		res.Recall5 = sumR5 / n
		res.Recall10 = sumR10 / n
		res.MRR10 = sumMRR / n
		res.NDCG10 = sumNDCG / n
	}
	return res, nil
}

func idsFromMemories(ms []memory.Memory, err error) ([]string, error) {
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(ms))
	for i, m := range ms {
		ids[i] = m.ID
	}
	return ids, nil
}

func idsFromScored(ss []memory.ScoredMemory, err error) ([]string, error) {
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(ss))
	for i, s := range ss {
		ids[i] = s.MemoryID
	}
	return ids, nil
}

// discard is an io.Writer that drops the linking worker's log output during a
// benchmark run.
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
