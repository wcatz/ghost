package bench

import (
	"context"
	"fmt"
	"math"

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
// has to be PAIRED — the same query under each — to have any power. On the
// committed dataset the two legs' per-query scores correlate at r = 0.88, and
// removing that is worth about 4× in interval width (half-width 0.015 paired
// against 0.063 unpaired), which is what made a margin that thin look like a
// result when it was compared as a difference of means.
type QueryScore struct {
	Name string
	NDCG float64
	MRR  float64
	// TopCosine is the best cosine among the rows this condition returned for
	// the query, scored from each row's own stored vector (see resultCosines).
	// It is carried here rather than searched for again by the no-answer
	// report's answerable contrast, which is the same hybrid search this one
	// already made.
	TopCosine float64
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
	// TopRowRelevant and TopRowBestLabelled are the shares of the scored
	// queries whose top-ranked row is relevant, and whose top-ranked row
	// carries the best label the query gives any row. They answer the
	// question the R@1 column is usually read as — "did the first row I
	// was handed answer me" — which RecallAtK does not measure: recall@1
	// divides by the number of labelled rows, so on a query grading four
	// it scores 0.250 for a perfect ranking. Both are fractions of the same
	// scored queries, so they are read against that population and not
	// against the window.
	TopRowRelevant     float64
	TopRowBestLabelled float64
	// Recall1Ceiling is what a perfect ranking of this query set scores at
	// R@1 — the mean of 1/(labelled rows per query). A property of the
	// LABELS rather than of the ranking, so it is one figure for the run
	// and identical over every condition that scored the same query set.
	// It is why an R@1 of 0.520 on a corpus where 163 of 220 queries grade
	// 2-4 rows is 87% of what is reachable, not 52% of it.
	Recall1Ceiling float64
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
	var sumTopRel, sumTopBest, sumCeiling float64
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
		// Computed once and used twice: the aggregate sums and the per-query
		// record are the same two numbers, and calling NDCGAtK twice per query
		// doubled the metric cost of every run in this package for nothing.
		ndcg := NDCGAtK(ranked, q.Rel, 10)
		mrr := ReciprocalRankAtK(ranked, q.Rel, 10)
		sumR1 += RecallAtK(ranked, q.Rel, 1)
		sumR5 += RecallAtK(ranked, q.Rel, 5)
		sumR10 += RecallAtK(ranked, q.Rel, 10)
		sumMRR += mrr
		sumNDCG += ndcg
		// The top-row shares read the same ranked list the five sums
		// above just read — one list per query, both questions about its
		// head. The ceiling is a property of the labels alone and is
		// summed over the same loop so a query one condition measured
		// and another did not cannot shift the denominator.
		sumTopRel += boolAsFloat(TopRowRelevant(ranked, q.Rel))
		sumTopBest += boolAsFloat(TopRowBestLabelled(ranked, q.Rel))
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
		res.Recall1Ceiling = sumCeiling / n
	}
	return res, nil
}

// boolAsFloat renders a per-query verdict as the 0 or 1 a share sums
// over. A share is a mean over queries, so each query contributes one
// unit and the sum is a count.
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
