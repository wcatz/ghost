// Package bench provides a retrieval-quality benchmark harness for Ghost's
// search. It drives the real FTS/vector/hybrid code paths over an
// in-memory store and scores the ranked results with judge-free IR metrics.
//
// See docs/benchmarks.md for methodology. All metrics here are pure functions
// of a ranked result list and a graded-relevance map, so they are fully
// deterministic and need no LLM judge.
package bench

import (
	"math"
	"sort"
)

// Relevance maps a memory ID to its relevance gain for a query. A gain of 0
// (or an absent ID) means not relevant; higher gains rank higher in the ideal
// ordering used by NDCG. Binary-relevance datasets use gain 1 for every
// relevant ID.
type Relevance map[string]int

// relevantCount returns the number of IDs with a positive gain.
func (r Relevance) relevantCount() int {
	n := 0
	for _, g := range r {
		if g > 0 {
			n++
		}
	}
	return n
}

// RecallAtK is the fraction of all relevant items that appear in the top-k
// results. Returns 0 when the query has no relevant items (an undefined ratio),
// so callers should exclude no-relevant queries from aggregate recall.
func RecallAtK(ranked []string, rel Relevance, k int) float64 {
	total := rel.relevantCount()
	if total == 0 {
		return 0
	}
	hit := 0
	for i, id := range ranked {
		if i >= k {
			break
		}
		if rel[id] > 0 {
			hit++
		}
	}
	return float64(hit) / float64(total)
}

// ReciprocalRankAtK returns 1/rank of the first relevant result within the
// top-k (rank is 1-based), or 0 if none of the top-k are relevant. Averaging
// this across queries yields MRR@k.
func ReciprocalRankAtK(ranked []string, rel Relevance, k int) float64 {
	for i, id := range ranked {
		if i >= k {
			break
		}
		if rel[id] > 0 {
			return 1.0 / float64(i+1)
		}
	}
	return 0
}

// NDCGAtK is the normalized discounted cumulative gain over the top-k, using
// the standard log2 discount: DCG = Σ gain_i / log2(i+2) for i in 0..k-1,
// normalized by the ideal DCG (relevant items sorted by descending gain).
// Returns 0 when the query has no relevant items.
func NDCGAtK(ranked []string, rel Relevance, k int) float64 {
	ideal := idealDCG(rel, k)
	if ideal == 0 {
		return 0
	}
	var dcg float64
	for i, id := range ranked {
		if i >= k {
			break
		}
		if g := rel[id]; g > 0 {
			dcg += float64(g) / math.Log2(float64(i+2))
		}
	}
	return dcg / ideal
}

// idealDCG computes the DCG of the best possible ranking: all relevant gains
// sorted descending, truncated to k.
func idealDCG(rel Relevance, k int) float64 {
	gains := make([]int, 0, len(rel))
	for _, g := range rel {
		if g > 0 {
			gains = append(gains, g)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(gains)))
	var idcg float64
	for i, g := range gains {
		if i >= k {
			break
		}
		idcg += float64(g) / math.Log2(float64(i+2))
	}
	return idcg
}
