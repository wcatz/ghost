// Package bench provides retrieval-quality benchmark harness for Ghost's search.
package bench

import (
	"math"
	"sort"
)

// Relevance maps a memory ID to its relevance gain for a query.
type Relevance map[string]int

func (r Relevance) relevantCount() int {
	n := 0
	for _, g := range r {
		if g > 0 {
			n++
		}
	}
	return n
}

// RecallAtK is the fraction of all relevant items in the top-k results.
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

// TopRowRelevant reports whether the first ranked result is relevant.
func TopRowRelevant(ranked []string, rel Relevance) bool {
	if len(ranked) == 0 {
		return false
	}
	return rel[ranked[0]] > 0
}

// AnyRelevantInTopK reports whether any of the first k results is relevant.
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

// TopRowBestLabelled reports whether the first result has the highest gain.
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

// Recall1Ceiling is the maximum RecallAtK at k=1 for the relevance labels.
func Recall1Ceiling(rel Relevance) float64 {
	total := rel.relevantCount()
	if total == 0 {
		return 0
	}
	return 1.0 / float64(total)
}

// ReciprocalRankAtK returns 1/rank of the first relevant result in the top-k.
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

// NDCGAtK is the normalized discounted cumulative gain over the top-k results.
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
