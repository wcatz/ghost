package bench

import (
	"context"
	"fmt"

	"github.com/wcatz/ghost/internal/memory"
)

// scoreK is the retrieval depth: results are scored at recall@1/5/10, so the
// search must return at least this many candidates.
const scoreK = 10

// Query is one benchmark question: its text, an optional precomputed embedding
// (required for the vector and hybrid conditions), the project to search, and
// the graded relevance of memory IDs. Flavor is set only for no-answer queries
// (see falsepositive.go) and is empty for a graded one.
type Query struct {
	Name      string
	ProjectID string
	Text      string
	Vector    []float32
	Rel       Relevance
	Flavor    string
}

// QueryScore is one graded query's score under one condition. It is kept per
// query rather than only as a mean because a comparison between two conditions
// has to be PAIRED — the same query under each — to have any power. An unpaired
// comparison of two means over 220 queries carries a confidence interval an
// order of magnitude wider than the difference being claimed, which is what made
// a 0.017 margin look like a result.
type QueryScore struct {
	Name string
	NDCG float64
	MRR  float64
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
	// PerQuery holds this condition's score for every scored query, in
	// query-set order. A slice rather than a map because the pairing is
	// positional and a map would let a missing or duplicated key line two
	// different queries up and report a difference between them.
	PerQuery []QueryScore
	// NoAnswer holds what this condition returned for the query set's
	// no-answer queries — the ones with an empty relevance map, which are
	// undefined for every ratio above and are therefore measured instead of
	// averaged in. It is per condition rather than one block for the run
	// because the legs fail differently: a vector leg with no floor will put
	// a weak match confidently at the top where a keyword leg puts a long row
	// with one incidental term there, and a single number taken from the
	// shipped hybrid path hides which of the two did it.
	//
	// Report-only, and that is a decision: the rate is a claim about today's
	// ranking, and the abstention fix this baseline exists for would move it.
	NoAnswer []NoAnswerQuery
}

// NoAnswerQuery is one no-answer query's result under one condition: how many
// rows came back, the best cosine among them, and every returned row's own
// cosine. The per-row values are kept rather than just the aggregate because
// the report's floors and its maximum are properties of the distribution — an
// average cannot supply either, and a pooled mean across flavors would let the
// easy flavor carry the hard one.
type NoAnswerQuery struct {
	Name    string
	Flavor  string
	Results int
	// Top is the best cosine among the returned rows; 0 when the window was
	// empty. Every score is the row's own cosine read from its stored vector
	// (see resultCosines), because a hybrid result can arrive on the keyword
	// leg alone and has no score of its own to read elsewhere.
	Top     float64
	Cosines map[string]float32
}

// Condition names, stable for reporting.
const (
	CondFTS    = "fts-only"
	CondVector = "vector-only"
	CondHybrid = "hybrid"
)

// Run evaluates the fts, vector, and hybrid ablations over the seeded store and
// query set. Queries with an empty relevance map are no-answer queries: they are
// excluded from every graded ratio (which is arithmetic, not a choice) and
// measured as false positives instead (see Result.NoAnswer), so nothing in the
// set is silently dropped.
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

// rankFn returns the ranked memory IDs for one query under a condition.
type rankFn func(q Query) ([]string, error)

func runCondition(ctx context.Context, store *memory.Store, name string, queries []Query, rank rankFn) (Result, error) {
	res := Result{Condition: name}
	var sumR1, sumR5, sumR10, sumMRR, sumNDCG float64
	for _, q := range queries {
		if q.Rel.relevantCount() == 0 {
			// A query nothing in the corpus answers is undefined for these
			// ratios, so it cannot be scored — but it is the case a wrong
			// memory returned for, and a wrong memory returned counts as a hit
			// for whatever it displaced. Skipping it is what let a
			// confidently-wrong system score well, so it is measured instead.
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
		sumR1 += RecallAtK(ranked, q.Rel, 1)
		sumR5 += RecallAtK(ranked, q.Rel, 5)
		sumR10 += RecallAtK(ranked, q.Rel, 10)
		sumMRR += ReciprocalRankAtK(ranked, q.Rel, 10)
		sumNDCG += NDCGAtK(ranked, q.Rel, 10)
		res.PerQuery = append(res.PerQuery, QueryScore{
			Name: q.Name,
			NDCG: NDCGAtK(ranked, q.Rel, 10),
			MRR:  ReciprocalRankAtK(ranked, q.Rel, 10),
		})
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
