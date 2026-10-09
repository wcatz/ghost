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

// TopRowRelevant reports whether the row a search put first is one the
// query grades relevant. It is a statement about ONE row, not about how
// many relevant rows the window holds, so a query that labels four
// rows and puts one of them first scores 1 here and 0.25 at
// RecallAtK 1 — which is the difference between "the first row
// answered me" and the share RecallAtK actually measures. An empty
// list has no top row, so it reports false rather than panicking on
// ranked[0].
func TopRowRelevant(ranked []string, rel Relevance) bool {
	if len(ranked) == 0 {
		return false
	}
	return rel[ranked[0]] > 0
}

// AnyRelevantInTopK reports whether at least one of the first k rows is
// one the query grades relevant. It is the share-friendly cousin of
// RecallAtK: a query that labels four rows and has one of them in the
// window scores 1 here and 0.25 there, so it answers "did the window
// hold something that answers me" rather than "what fraction of the
// answer was found". A k below 1 or an empty list has no window, so it
// reports false.
func AnyRelevantInTopK(ranked []string, rel Relevance, k int) bool {
	if k > len(ranked) {
		k = len(ranked)
	}
	for i := 0; i < k; i++ {
		if rel[ranked[i]] > 0 {
			return true
		}
	}
	return false
}

// TopRowBestLabelled reports whether the top row carries the highest
// gain the query labels any row with. A tie for the best label counts:
// two rows at the same gain are both best-labelled, so a query grading
// {a:1, b:1} with a first reports true, because nothing the query
// labels was ranked above it. A top row with no positive gain is never
// best-labelled — it is not a labelled row at all.
func TopRowBestLabelled(ranked []string, rel Relevance) bool {
	if len(ranked) == 0 {
		return false
	}
	g := rel[ranked[0]]
	if g <= 0 {
		return false
	}
	for _, gain := range rel {
		if gain > g {
			return false
		}
	}
	return true
}

// Recall1Ceiling is the RecallAtK(ranked, rel, 1) no ranking of the
// query can beat: rank 1 holds one row, so a query labelling N rows
// scores at most 1/N, however good the ranking is. It is a property of
// the LABELS, not of any ranking, which is why it takes no ranked list
// — averaged over a query set it is the score a perfect ranking posts
// at R@1, and it is what every R@1 has to be read against, because a
// corpus labelling 2-4 rows per query caps a perfect R@1 well below 1
// and a plain R@1 number cannot tell that from a ranking failure.
// Undefined for a query with no labelled rows, like every other ratio
// here, so callers exclude those from the mean.
func Recall1Ceiling(rel Relevance) float64 {
	total := rel.relevantCount()
	if total == 0 {
		return 0
	}
	return 1.0 / float64(total)
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
