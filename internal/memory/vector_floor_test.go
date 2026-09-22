package memory

import (
	"context"
	"testing"
)

// TestSearchHybridAppliesVectorMinSimilarity: a configured cosine floor must
// drop weak vector candidates before fusion so they cannot pad the result
// list; FTS-only fallback still works when the floor empties the vector leg.
func TestSearchHybridAppliesVectorMinSimilarity(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Orthogonal-ish embeddings: strong match ~1.0, weak match ~0.1 cosine.
	// Content is deliberately disjoint from the query so FTS cannot rescue
	// the weak candidate — the floor is vector-only and this test must prove
	// the vector leg drops it.
	strong := makeMemory(t, s, "kubernetes readiness probe misconfiguration")
	weak := makeMemory(t, s, "zzz unrelated quantum banana syntax")

	q := []float32{1, 0, 0}
	if err := s.StoreEmbedding(ctx, strong, []float32{1, 0, 0}, "test"); err != nil {
		t.Fatalf("StoreEmbedding strong: %v", err)
	}
	// cosine with {1,0,0} of {0.1, 0.99, 0} ≈ 0.1 — above 0, below a 0.5 floor
	if err := s.StoreEmbedding(ctx, weak, []float32{0.1, 0.99, 0}, "test"); err != nil {
		t.Fatalf("StoreEmbedding weak: %v", err)
	}

	// Baseline: floor 0 keeps both (weak cosine > 0). Query terms only match
	// strong lexically; weak arrives via the vector leg.
	p := DefaultSearchParams()
	p.MinSimilarity = 0
	got, err := s.SearchHybridParams(ctx, testProject, "kubernetes readiness probe", q, 10, p)
	if err != nil {
		t.Fatalf("SearchHybridParams baseline: %v", err)
	}
	var sawWeakBaseline bool
	for _, m := range got {
		if m.ID == weak {
			sawWeakBaseline = true
		}
	}
	if !sawWeakBaseline {
		t.Fatalf("baseline must retrieve weak via vector leg (cosine ~0.1); got %+v", got)
	}

	// Floor 0.5 drops the weak candidate from the vector leg. Iterate the
	// full slice: an early return on seeing strong would skip the weak check
	// whenever strong ranks first (the common case under RRF).
	p.MinSimilarity = 0.5
	got, err = s.SearchHybridParams(ctx, testProject, "kubernetes readiness probe", q, 10, p)
	if err != nil {
		t.Fatalf("SearchHybridParams floored: %v", err)
	}
	var sawStrong bool
	for _, m := range got {
		if m.ID == weak {
			t.Errorf("weak candidate %s must be dropped by MinSimilarity=0.5", weak)
		}
		if m.ID == strong {
			sawStrong = true
		}
	}
	if !sawStrong {
		t.Errorf("strong candidate must survive the floor; got %+v", got)
	}
}

// TestStoreVectorMinSimilarityDefaultIsZero: unset store keeps historical
// behavior (floor 0).
func TestStoreVectorMinSimilarityDefaultIsZero(t *testing.T) {
	s := testStore(t)
	if got := s.vectorMinSimilarityFloor(); got != 0 {
		t.Errorf("default floor = %v, want 0", got)
	}
	s.SetVectorMinSimilarity(0.25)
	if got := s.vectorMinSimilarityFloor(); got != 0.25 {
		t.Errorf("SetVectorMinSimilarity(0.25) = %v, want 0.25", got)
	}
}

func TestFilterVectorFloor(t *testing.T) {
	in := []ScoredMemory{
		{MemoryID: "a", Score: 0.9},
		{MemoryID: "b", Score: 0.4},
		{MemoryID: "c", Score: 0.1},
	}
	if got := filterVectorFloor(in, 0); len(got) != 3 {
		t.Errorf("floor 0: got %d, want 3", len(got))
	}
	got := filterVectorFloor(in, 0.5)
	if len(got) != 1 || got[0].MemoryID != "a" {
		t.Errorf("floor 0.5: got %+v, want only a", got)
	}
	// floor above everything → empty (caller falls back to FTS-only)
	if got := filterVectorFloor(in, 0.99); len(got) != 0 {
		t.Errorf("floor 0.99: got %d, want 0", len(got))
	}
}
