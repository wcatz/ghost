package bench

import (
	"context"
	"fmt"
	"math"

	"github.com/wcatz/ghost/internal/memory"
)

const scoreK = 10

// Query is one benchmark question.
type Query struct {
	Name      string
	ProjectID string
	Text      string
	Vector    []float32
	Rel       Relevance
	Flavor    string
}

// QueryScore is one graded query's score under one condition.
type QueryScore struct {
	Name      string
	NDCG      float64
	MRR       float64
	TopCosine float64
}

// Result holds aggregate metrics for one search condition over a query set.
type Result struct {
	Condition          string
	Queries            int
	Recall1            float64
	Recall5            float64
	Recall10           float64
	MRR10              float64
	NDCG10             float64
	TopRowRelevant     float64
	TopRowBestLabelled float64
	Top5Relevant       float64
	Recall1Ceiling     float64
	PerQuery           []QueryScore
	NoAnswer           []NoAnswerQuery
}

// NoAnswerQuery is one no-answer query's result under one condition.
type NoAnswerQuery struct {
	Name    string
	Flavor  string
	Results int
	Top     float64
	Cosines map[string]float32
}

// Condition names.
const (
	CondFTS    = "fts-only"
	CondVector = "vector-only"
	CondHybrid = "hybrid"
)

// Run evaluates the fts, vector, and hybrid conditions over the query set.
func Run(ctx context.Context, store *memory.Store, queries []Query) ([]Result, error) {
	fts, err := runCondition(ctx, store, CondFTS, queries, func(q Query) ([]string, error) {
		return idsFromMemories(store.SearchFTS(ctx, q.ProjectID, q.Text, scoreK))
	})
	if err != nil {
		return nil, err
	}
	vec, err := runCondition(ctx, store, CondVector, queries, func(q Query) ([]string, error) {
		return idsFromScored(store.SearchVector(ctx, q.ProjectID, q.Vector, scoreK))
	})
	if err != nil {
		return nil, err
	}
	hybrid, err := runCondition(ctx, store, CondHybrid, queries, func(q Query) ([]string, error) {
		return idsFromMemories(store.SearchHybrid(ctx, q.ProjectID, q.Text, q.Vector, scoreK))
	})
	if err != nil {
		return nil, err
	}

	return []Result{fts, vec, hybrid}, nil
}

type rankFn func(q Query) ([]string, error)

func runCondition(ctx context.Context, store *memory.Store, name string, queries []Query, rank rankFn) (Result, error) {
	res := Result{Condition: name}
	var sumR1, sumR5, sumR10, sumMRR, sumNDCG float64
	var sumTopRel, sumTopBest, sumTop5, sumCeiling float64
	for _, q := range queries {
		if q.Rel.relevantCount() == 0 {
			measured, err := measureNoAnswer(ctx, store, q, rank)
			if err != nil {
				return Result{}, fmt.Errorf("%s: no-answer query %q: %w", name, q.Name, err)
			}
			res.NoAnswer = append(res.NoAnswer, measured)
			continue
		}
		ranked, err := rank(q)
		if err != nil {
			return Result{}, fmt.Errorf("%s: query %q: %w", name, q.Name, err)
		}
		res.Queries++
		ndcg := NDCGAtK(ranked, q.Rel, 10)
		mrr := ReciprocalRankAtK(ranked, q.Rel, 10)
		sumR1 += RecallAtK(ranked, q.Rel, 1)
		sumR5 += RecallAtK(ranked, q.Rel, 5)
		sumR10 += RecallAtK(ranked, q.Rel, 10)
		sumMRR += mrr
		sumNDCG += ndcg
		sumTopRel += boolAsFloat(TopRowRelevant(ranked, q.Rel))
		sumTopBest += boolAsFloat(TopRowBestLabelled(ranked, q.Rel))
		sumTop5 += boolAsFloat(AnyRelevantInTopK(ranked, q.Rel, 5))
		sumCeiling += Recall1Ceiling(q.Rel)
		cosines, err := resultCosines(ctx, store, q.Vector, ranked)
		if err != nil {
			return Result{}, fmt.Errorf("%s: query %q: %w", name, q.Name, err)
		}
		var top float64
		for _, c := range cosines {
			top = math.Max(top, float64(c))
		}
		res.PerQuery = append(res.PerQuery, QueryScore{Name: q.Name, NDCG: ndcg, MRR: mrr, TopCosine: top})
	}
	if res.Queries > 0 {
		n := float64(res.Queries)
		res.Recall1 = sumR1 / n
		res.Recall5 = sumR5 / n
		res.Recall10 = sumR10 / n
		res.MRR10 = sumMRR / n
		res.NDCG10 = sumNDCG / n
		res.TopRowRelevant = sumTopRel / n
		res.TopRowBestLabelled = sumTopBest / n
		res.Top5Relevant = sumTop5 / n
		res.Recall1Ceiling = sumCeiling / n
	}
	return res, nil
}

func boolAsFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
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
