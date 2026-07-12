package bench

import (
	"math"
	"testing"
)

func approx(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-6 {
		t.Errorf("%s = %.6f, want %.6f", name, got, want)
	}
}

func TestRecallAtK(t *testing.T) {
	ranked := []string{"a", "b", "c", "d"}
	rel := Relevance{"a": 1, "c": 1, "x": 1} // 3 relevant, x not in results

	approx(t, "recall@3", RecallAtK(ranked, rel, 3), 2.0/3.0) // a,c in top3
	approx(t, "recall@10", RecallAtK(ranked, rel, 10), 2.0/3.0)
	approx(t, "recall@1", RecallAtK(ranked, rel, 1), 1.0/3.0) // only a
	approx(t, "recall@0", RecallAtK(ranked, rel, 0), 0)       // no window
	approx(t, "no-relevant", RecallAtK(ranked, Relevance{}, 5), 0)
	// gain 0 entries do not count as relevant
	approx(t, "zero-gain-ignored", RecallAtK([]string{"z"}, Relevance{"z": 0}, 5), 0)
}

func TestReciprocalRankAtK(t *testing.T) {
	approx(t, "first-at-2", ReciprocalRankAtK([]string{"b", "a", "c"}, Relevance{"a": 1}, 3), 1.0/2.0)
	approx(t, "first-at-3", ReciprocalRankAtK([]string{"b", "x", "c"}, Relevance{"c": 1}, 3), 1.0/3.0)
	approx(t, "none-in-results", ReciprocalRankAtK([]string{"b", "x"}, Relevance{"a": 1}, 3), 0)
	approx(t, "relevant-beyond-k", ReciprocalRankAtK([]string{"b", "a"}, Relevance{"a": 1}, 1), 0)
	approx(t, "first-at-1", ReciprocalRankAtK([]string{"a", "b"}, Relevance{"a": 1, "b": 1}, 5), 1.0)
}

func TestNDCGAtK(t *testing.T) {
	// Perfect binary ranking → 1.0.
	approx(t, "perfect-binary", NDCGAtK([]string{"a", "b"}, Relevance{"a": 1, "b": 1}, 2), 1.0)

	// Graded, imperfect order: rel a=3,b=1; ranked [b,a].
	// DCG  = gain(b)/log2(2) + gain(a)/log2(3) = 1/1 + 3/log2(3)
	// IDCG = 3/log2(2) + 1/log2(3)             = 3/1 + 1/log2(3)
	gradedReversed := (1.0/math.Log2(2) + 3.0/math.Log2(3)) / (3.0/math.Log2(2) + 1.0/math.Log2(3))
	approx(t, "graded-reversed", NDCGAtK([]string{"b", "a"}, Relevance{"a": 3, "b": 1}, 2), gradedReversed)

	// Graded, ideal order → 1.0.
	approx(t, "graded-ideal", NDCGAtK([]string{"a", "b"}, Relevance{"a": 3, "b": 1}, 2), 1.0)

	// Relevant item pushed past k contributes nothing.
	// ranked [x,a], rel {a:1}, k=1 → DCG=0 → NDCG=0.
	approx(t, "relevant-past-k", NDCGAtK([]string{"x", "a"}, Relevance{"a": 1}, 1), 0)

	// No relevant items → 0, not NaN.
	approx(t, "no-relevant", NDCGAtK([]string{"a"}, Relevance{}, 5), 0)

	// A single relevant at rank 2 (binary): DCG = 1/log2(3)=0.630930,
	// IDCG (1 relevant) = 1/log2(2) = 1 → NDCG = 0.630930.
	approx(t, "single-at-2", NDCGAtK([]string{"x", "a", "y"}, Relevance{"a": 1}, 10), 0.630930)
}

func TestRelevantCount(t *testing.T) {
	if n := (Relevance{"a": 1, "b": 0, "c": 2}).relevantCount(); n != 2 {
		t.Errorf("relevantCount = %d, want 2", n)
	}
}
