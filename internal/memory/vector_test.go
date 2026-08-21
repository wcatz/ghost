package memory

import (
	"context"
	"math"
	"testing"
	"time"
)

func TestDecayFactor_MatchesSQLSemantics(t *testing.T) {
	cases := []struct {
		category string
		pinned   bool
		ageDays  float64
		want     float64
	}{
		{"decision", false, 0, 1.0},                    // brand new → no decay
		{"decision", false, 30, 0.5},                   // tau=30: 1/(1+30/30)
		{"gotcha", false, 30, 0.5},                     // same tier as decision
		{"dependency", false, 90, 0.25},                // tau=30: 1/(1+90/30), above 0.15 floor
		{"pattern", false, 45, 0.5},                    // tau=45: 1/(1+45/45)
		{"architecture", false, 100, 45.0 / 145.0},     // tau=45: 1/(1+100/45), above 0.3 floor
		{"preference", false, 1000, 1.0},               // never decays
		{"convention", false, 1000, 1.0},               // never decays
		{"fact", false, 1000, 1.0},                     // never decays
		{"decision", true, 1000, 1.0},                  // pinned → exempt
		{"gotcha", true, 30, 1.0},                      // pinned beats decay
		{"decision", false, -5, 1.2},                   // negative age: SQL doesn't clamp, factor > 1
	}
	const eps = 1e-9
	for _, c := range cases {
		got := decayFactor(c.category, c.pinned, c.ageDays)
		if math.Abs(got-c.want) > eps {
			t.Errorf("decayFactor(%q, pinned=%v, age=%.1f) = %v, want %v",
				c.category, c.pinned, c.ageDays, got, c.want)
		}
	}
}

// --- float32sToBytes / bytesToFloat32s roundtrip tests ---

func TestVectorRoundtrip_Empty(t *testing.T) {
	got := bytesToFloat32s(float32sToBytes(nil))
	if len(got) != 0 {
		t.Fatalf("expected empty slice, got %v", got)
	}
}

func TestVectorRoundtrip_Single(t *testing.T) {
	input := []float32{3.14}
	got := bytesToFloat32s(float32sToBytes(input))
	if len(got) != 1 || got[0] != input[0] {
		t.Fatalf("expected %v, got %v", input, got)
	}
}

func TestVectorRoundtrip_Many(t *testing.T) {
	input := []float32{1.0, -1.0, 0.0, 0.5, -0.5, 100.0, -100.0, 1e-10, 1e10}
	got := bytesToFloat32s(float32sToBytes(input))
	if len(got) != len(input) {
		t.Fatalf("length mismatch: expected %d, got %d", len(input), len(got))
	}
	for i := range input {
		if got[i] != input[i] {
			t.Errorf("index %d: expected %v, got %v", i, input[i], got[i])
		}
	}
}

func TestVectorRoundtrip_Negative(t *testing.T) {
	input := []float32{-42.0, -0.001, -1e5}
	got := bytesToFloat32s(float32sToBytes(input))
	for i := range input {
		if got[i] != input[i] {
			t.Errorf("index %d: expected %v, got %v", i, input[i], got[i])
		}
	}
}

func TestVectorRoundtrip_Zero(t *testing.T) {
	input := []float32{0.0, 0.0, 0.0}
	got := bytesToFloat32s(float32sToBytes(input))
	for i := range input {
		if got[i] != 0 {
			t.Errorf("index %d: expected 0, got %v", i, got[i])
		}
	}
}

func TestVectorRoundtrip_NaN(t *testing.T) {
	input := []float32{float32(math.NaN())}
	got := bytesToFloat32s(float32sToBytes(input))
	if len(got) != 1 {
		t.Fatalf("expected 1 element, got %d", len(got))
	}
	if !math.IsNaN(float64(got[0])) {
		t.Fatalf("expected NaN, got %v", got[0])
	}
}

func TestVectorRoundtrip_Inf(t *testing.T) {
	input := []float32{float32(math.Inf(1)), float32(math.Inf(-1))}
	got := bytesToFloat32s(float32sToBytes(input))
	if !math.IsInf(float64(got[0]), 1) {
		t.Errorf("expected +Inf, got %v", got[0])
	}
	if !math.IsInf(float64(got[1]), -1) {
		t.Errorf("expected -Inf, got %v", got[1])
	}
}

func TestVectorBytesLength(t *testing.T) {
	input := []float32{1.0, 2.0, 3.0}
	b := float32sToBytes(input)
	if len(b) != len(input)*4 {
		t.Fatalf("expected %d bytes, got %d", len(input)*4, len(b))
	}
}

// --- cosineSimilarity tests ---

func TestVectorCosine_Identical(t *testing.T) {
	v := []float32{1.0, 2.0, 3.0}
	sim := cosineSimilarity(v, v)
	if diff := math.Abs(float64(sim) - 1.0); diff > 1e-6 {
		t.Fatalf("expected ~1.0, got %v", sim)
	}
}

func TestVectorCosine_Orthogonal(t *testing.T) {
	a := []float32{1.0, 0.0}
	b := []float32{0.0, 1.0}
	sim := cosineSimilarity(a, b)
	if diff := math.Abs(float64(sim)); diff > 1e-6 {
		t.Fatalf("expected ~0.0, got %v", sim)
	}
}

func TestVectorCosine_Opposite(t *testing.T) {
	a := []float32{1.0, 0.0, 0.0}
	b := []float32{-1.0, 0.0, 0.0}
	sim := cosineSimilarity(a, b)
	if diff := math.Abs(float64(sim) + 1.0); diff > 1e-6 {
		t.Fatalf("expected ~-1.0, got %v", sim)
	}
}

func TestVectorCosine_ZeroVector(t *testing.T) {
	a := []float32{0.0, 0.0, 0.0}
	b := []float32{1.0, 2.0, 3.0}
	if sim := cosineSimilarity(a, b); sim != 0 {
		t.Fatalf("expected 0 for zero vector, got %v", sim)
	}
	if sim := cosineSimilarity(b, a); sim != 0 {
		t.Fatalf("expected 0 for zero vector (swapped), got %v", sim)
	}
}

func TestVectorCosine_BothZero(t *testing.T) {
	a := []float32{0.0, 0.0}
	if sim := cosineSimilarity(a, a); sim != 0 {
		t.Fatalf("expected 0 for two zero vectors, got %v", sim)
	}
}

func TestVectorCosine_MismatchedLengths(t *testing.T) {
	a := []float32{1.0, 2.0}
	b := []float32{1.0, 2.0, 3.0}
	if sim := cosineSimilarity(a, b); sim != 0 {
		t.Fatalf("expected 0 for mismatched lengths, got %v", sim)
	}
}

func TestVectorCosine_EmptyVectors(t *testing.T) {
	if sim := cosineSimilarity(nil, nil); sim != 0 {
		t.Fatalf("expected 0 for nil vectors, got %v", sim)
	}
	if sim := cosineSimilarity([]float32{}, []float32{}); sim != 0 {
		t.Fatalf("expected 0 for empty vectors, got %v", sim)
	}
}

func TestVectorCosine_KnownAngle(t *testing.T) {
	// 45 degrees: cos(45) = 1/sqrt(2) ≈ 0.7071
	a := []float32{1.0, 0.0}
	b := []float32{1.0, 1.0}
	sim := cosineSimilarity(a, b)
	expected := float32(1.0 / math.Sqrt(2.0))
	if diff := math.Abs(float64(sim - expected)); diff > 1e-6 {
		t.Fatalf("expected ~%v, got %v", expected, sim)
	}
}

// --- helpers for DB-backed tests ---

func setupTestStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store := NewStore(db, nil)
	ctx := context.Background()

	if err := store.EnsureProject(ctx, "test-proj", "/test", "test"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	return store, ctx
}

func createTestMemory(t *testing.T, store *Store, ctx context.Context, content string) string {
	t.Helper()
	id, err := store.Create(ctx, "test-proj", Memory{
		Category:   "fact",
		Content:    content,
		Source:     "manual",
		Importance: 0.8,
		Tags:       []string{"test"},
	})
	if err != nil {
		t.Fatalf("Create memory: %v", err)
	}
	return id
}

// --- StoreEmbedding + SearchVector roundtrip ---

func TestVectorStoreAndSearch(t *testing.T) {
	store, ctx := setupTestStore(t)

	id1 := createTestMemory(t, store, ctx, "Go is a compiled language")
	id2 := createTestMemory(t, store, ctx, "Python is an interpreted language")
	id3 := createTestMemory(t, store, ctx, "Rust has zero-cost abstractions")

	// Store embeddings — make id1's vector similar to the query.
	vec1 := []float32{0.9, 0.1, 0.0}
	vec2 := []float32{0.1, 0.9, 0.0}
	vec3 := []float32{0.0, 0.1, 0.9}

	for _, tc := range []struct {
		id  string
		vec []float32
	}{
		{id1, vec1},
		{id2, vec2},
		{id3, vec3},
	} {
		if err := store.StoreEmbedding(ctx, tc.id, tc.vec, "test-model"); err != nil {
			t.Fatalf("StoreEmbedding(%s): %v", tc.id, err)
		}
	}

	// Query vector close to vec1.
	query := []float32{0.95, 0.05, 0.0}
	results, err := store.SearchVector(ctx, "test-proj", query, 3)
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}

	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}

	// First result should be id1 (most similar to query).
	if results[0].MemoryID != id1 {
		t.Errorf("expected first result to be %s, got %s", id1, results[0].MemoryID)
	}

	// Scores should be in descending order.
	for i := 1; i < len(results); i++ {
		if results[i].Score > results[i-1].Score {
			t.Errorf("results not sorted: score[%d]=%v > score[%d]=%v",
				i, results[i].Score, i-1, results[i-1].Score)
		}
	}
}

func TestVectorSearchLimit(t *testing.T) {
	store, ctx := setupTestStore(t)

	// Create 5 memories with embeddings.
	for i := range 5 {
		id := createTestMemory(t, store, ctx, "memory content")
		vec := make([]float32, 3)
		vec[i%3] = 1.0
		if err := store.StoreEmbedding(ctx, id, vec, "test-model"); err != nil {
			t.Fatalf("StoreEmbedding: %v", err)
		}
	}

	query := []float32{1.0, 0.0, 0.0}
	results, err := store.SearchVector(ctx, "test-proj", query, 2)
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	if len(results) > 2 {
		t.Fatalf("expected at most 2 results, got %d", len(results))
	}
}

func TestVectorSearchDimensionMismatch(t *testing.T) {
	store, ctx := setupTestStore(t)

	id := createTestMemory(t, store, ctx, "some memory")
	vec := []float32{1.0, 0.0, 0.0}
	if err := store.StoreEmbedding(ctx, id, vec, "test-model"); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}

	// Query with different dimension — should return no matches.
	query := []float32{1.0, 0.0}
	results, err := store.SearchVector(ctx, "test-proj", query, 10)
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results for mismatched dimensions, got %d", len(results))
	}
}

func TestVectorSearchEmptyDB(t *testing.T) {
	store, ctx := setupTestStore(t)

	query := []float32{1.0, 0.0, 0.0}
	results, err := store.SearchVector(ctx, "test-proj", query, 10)
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results for empty DB, got %d", len(results))
	}
}

// --- StoreEmbedding upsert behavior ---

func TestVectorStoreEmbedding_Upsert(t *testing.T) {
	store, ctx := setupTestStore(t)

	id := createTestMemory(t, store, ctx, "updatable memory")
	vec1 := []float32{1.0, 0.0, 0.0}
	vec2 := []float32{0.0, 1.0, 0.0}

	if err := store.StoreEmbedding(ctx, id, vec1, "model-a"); err != nil {
		t.Fatalf("StoreEmbedding first: %v", err)
	}
	// Store again with different vector — should upsert, not error.
	if err := store.StoreEmbedding(ctx, id, vec2, "model-b"); err != nil {
		t.Fatalf("StoreEmbedding second: %v", err)
	}

	// Search should find the updated vector.
	query := []float32{0.0, 1.0, 0.0}
	results, err := store.SearchVector(ctx, "test-proj", query, 1)
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	// Similarity to the updated vector should be ~1.0.
	if diff := math.Abs(float64(results[0].Score) - 1.0); diff > 1e-5 {
		t.Errorf("expected score ~1.0 after upsert, got %v", results[0].Score)
	}
}

// --- DeleteEmbedding ---

func TestVectorDeleteEmbedding(t *testing.T) {
	store, ctx := setupTestStore(t)

	id := createTestMemory(t, store, ctx, "to be deleted")
	vec := []float32{1.0, 0.0, 0.0}
	if err := store.StoreEmbedding(ctx, id, vec, "test-model"); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}

	if err := store.DeleteEmbedding(ctx, id); err != nil {
		t.Fatalf("DeleteEmbedding: %v", err)
	}

	// After deletion, search should return nothing.
	results, err := store.SearchVector(ctx, "test-proj", vec, 10)
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results after delete, got %d", len(results))
	}
}

func TestVectorDeleteEmbedding_Nonexistent(t *testing.T) {
	store, ctx := setupTestStore(t)

	// Deleting a nonexistent embedding should not error.
	if err := store.DeleteEmbedding(ctx, "nonexistent-id"); err != nil {
		t.Fatalf("DeleteEmbedding nonexistent: %v", err)
	}
}

// --- UnembeddedMemoryIDs ---

func TestVectorUnembeddedMemoryIDs(t *testing.T) {
	store, ctx := setupTestStore(t)

	id1 := createTestMemory(t, store, ctx, "embedded memory")
	id2 := createTestMemory(t, store, ctx, "unembedded memory")
	_ = id2

	// Embed only id1.
	if err := store.StoreEmbedding(ctx, id1, []float32{1.0}, "test-model"); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}

	ids, err := store.UnembeddedMemoryIDs(ctx, "test-proj", 10)
	if err != nil {
		t.Fatalf("UnembeddedMemoryIDs: %v", err)
	}

	if len(ids) != 1 {
		t.Fatalf("expected 1 unembedded ID, got %d", len(ids))
	}
	if ids[0] != id2 {
		t.Errorf("expected unembedded ID %s, got %s", id2, ids[0])
	}
}

func TestVectorUnembeddedMemoryIDs_AllEmbedded(t *testing.T) {
	store, ctx := setupTestStore(t)

	id := createTestMemory(t, store, ctx, "fully embedded")
	if err := store.StoreEmbedding(ctx, id, []float32{1.0}, "test-model"); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}

	ids, err := store.UnembeddedMemoryIDs(ctx, "test-proj", 10)
	if err != nil {
		t.Fatalf("UnembeddedMemoryIDs: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("expected 0 unembedded IDs, got %d", len(ids))
	}
}

func TestVectorUnembeddedMemoryIDs_Limit(t *testing.T) {
	store, ctx := setupTestStore(t)

	for range 5 {
		createTestMemory(t, store, ctx, "unembedded")
	}

	ids, err := store.UnembeddedMemoryIDs(ctx, "test-proj", 2)
	if err != nil {
		t.Fatalf("UnembeddedMemoryIDs: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 IDs with limit=2, got %d", len(ids))
	}
}

// --- GetMemoryContent ---

func TestVectorGetMemoryContent(t *testing.T) {
	store, ctx := setupTestStore(t)

	content := "the content of this memory"
	id := createTestMemory(t, store, ctx, content)

	got, err := store.GetMemoryContent(ctx, id)
	if err != nil {
		t.Fatalf("GetMemoryContent: %v", err)
	}
	if got != content {
		t.Fatalf("expected %q, got %q", content, got)
	}
}

func TestVectorGetMemoryContent_NotFound(t *testing.T) {
	store, ctx := setupTestStore(t)

	_, err := store.GetMemoryContent(ctx, "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent memory, got nil")
	}
}

// --- GetByIDs ---

func TestVectorGetByIDs_Valid(t *testing.T) {
	store, ctx := setupTestStore(t)

	id1 := createTestMemory(t, store, ctx, "memory one")
	id2 := createTestMemory(t, store, ctx, "memory two")

	memories, err := store.GetByIDs(ctx, []string{id1, id2})
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	if len(memories) != 2 {
		t.Fatalf("expected 2 memories, got %d", len(memories))
	}

	// Verify both IDs are present (order not guaranteed).
	found := map[string]bool{}
	for _, m := range memories {
		found[m.ID] = true
	}
	if !found[id1] || !found[id2] {
		t.Errorf("expected IDs %s and %s, got %v", id1, id2, found)
	}
}

func TestVectorGetByIDs_SomeInvalid(t *testing.T) {
	store, ctx := setupTestStore(t)

	id := createTestMemory(t, store, ctx, "valid memory")

	memories, err := store.GetByIDs(ctx, []string{id, "nonexistent-id"})
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	if len(memories) != 1 {
		t.Fatalf("expected 1 memory (skipping invalid), got %d", len(memories))
	}
	if memories[0].ID != id {
		t.Errorf("expected ID %s, got %s", id, memories[0].ID)
	}
}

func TestVectorGetByIDs_AllInvalid(t *testing.T) {
	store, ctx := setupTestStore(t)

	memories, err := store.GetByIDs(ctx, []string{"bad1", "bad2"})
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	if len(memories) != 0 {
		t.Fatalf("expected 0 memories for invalid IDs, got %d", len(memories))
	}
}

func TestVectorGetByIDs_Empty(t *testing.T) {
	store, ctx := setupTestStore(t)

	memories, err := store.GetByIDs(ctx, []string{})
	if err != nil {
		t.Fatalf("GetByIDs empty: %v", err)
	}
	if memories != nil {
		t.Fatalf("expected nil for empty ID list, got %v", memories)
	}
}

func TestVectorGetByIDs_Nil(t *testing.T) {
	store, ctx := setupTestStore(t)

	memories, err := store.GetByIDs(ctx, nil)
	if err != nil {
		t.Fatalf("GetByIDs nil: %v", err)
	}
	if memories != nil {
		t.Fatalf("expected nil for nil ID list, got %v", memories)
	}
}

// --- SearchHybrid ---

func TestVectorSearchHybrid_FTSOnly(t *testing.T) {
	store, ctx := setupTestStore(t)

	createTestMemory(t, store, ctx, "Go programming language concurrency")
	createTestMemory(t, store, ctx, "Python data science libraries")

	// queryVec=nil should fall back to FTS-only.
	results, err := store.SearchHybrid(ctx, "test-proj", "Go concurrency", nil, 10)
	if err != nil {
		t.Fatalf("SearchHybrid FTS-only: %v", err)
	}

	// Should find at least the Go memory via FTS.
	if len(results) == 0 {
		t.Fatal("expected at least 1 FTS result, got 0")
	}

	// First result should be the Go-related memory.
	foundGo := false
	for _, m := range results {
		if m.Content == "Go programming language concurrency" {
			foundGo = true
			break
		}
	}
	if !foundGo {
		t.Error("expected to find Go concurrency memory in FTS results")
	}
}

func TestVectorSearchHybrid_FTSOnlyLimit(t *testing.T) {
	store, ctx := setupTestStore(t)

	// Create several memories that match "test".
	for i := range 5 {
		createTestMemory(t, store, ctx, "test memory content number "+string(rune('A'+i)))
	}

	results, err := store.SearchHybrid(ctx, "test-proj", "test memory", nil, 2)
	if err != nil {
		t.Fatalf("SearchHybrid: %v", err)
	}
	if len(results) > 2 {
		t.Fatalf("expected at most 2 results with limit=2, got %d", len(results))
	}
}

func TestVectorSearchHybrid_WithVector(t *testing.T) {
	store, ctx := setupTestStore(t)

	id1 := createTestMemory(t, store, ctx, "Go goroutines and channels")
	id2 := createTestMemory(t, store, ctx, "Python asyncio event loop")
	_ = id2

	// Store embeddings.
	if err := store.StoreEmbedding(ctx, id1, []float32{0.9, 0.1}, "test-model"); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}
	if err := store.StoreEmbedding(ctx, id2, []float32{0.1, 0.9}, "test-model"); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}

	// Query with both FTS and vector.
	queryVec := []float32{0.95, 0.05}
	results, err := store.SearchHybrid(ctx, "test-proj", "goroutines", queryVec, 10)
	if err != nil {
		t.Fatalf("SearchHybrid: %v", err)
	}

	if len(results) == 0 {
		t.Fatal("expected at least 1 hybrid result, got 0")
	}

	// First result should be id1 (matches both FTS and vector).
	if results[0].ID != id1 {
		t.Errorf("expected first result %s, got %s", id1, results[0].ID)
	}
}

func TestVectorSearchHybrid_NoResults(t *testing.T) {
	store, ctx := setupTestStore(t)

	// No memories at all — should return empty, not error.
	results, err := store.SearchHybrid(ctx, "test-proj", "nonexistent query", nil, 10)
	if err != nil {
		t.Fatalf("SearchHybrid no results: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results, got %d", len(results))
	}
}

// TestSearchHybridParams: the parameterized entrypoint must (a) reproduce
// SearchHybrid exactly under DefaultSearchParams and (b) honor leg weights — an
// all-vector weighting ranks the vector match first and an all-FTS weighting
// ranks the keyword match first on the same store.
func TestSearchHybridParams(t *testing.T) {
	store, ctx := setupTestStore(t)

	// kw matches the FTS query; vec matches the query vector. Cross-wired
	// embeddings so the two legs disagree about the winner.
	kw := createTestMemory(t, store, ctx, "goroutines scheduler internals")
	vec := createTestMemory(t, store, ctx, "python event loop design")
	if err := store.StoreEmbedding(ctx, kw, []float32{0, 1}, "test-model"); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}
	if err := store.StoreEmbedding(ctx, vec, []float32{1, 0}, "test-model"); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}
	queryVec := []float32{1, 0}

	// (a) Defaults reproduce SearchHybrid ranking exactly.
	want, err := store.SearchHybrid(ctx, "test-proj", "goroutines", queryVec, 10)
	if err != nil {
		t.Fatalf("SearchHybrid: %v", err)
	}
	got, err := store.SearchHybridParams(ctx, "test-proj", "goroutines", queryVec, 10, DefaultSearchParams())
	if err != nil {
		t.Fatalf("SearchHybridParams: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("defaults mismatch: got %d results, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i].ID {
			t.Errorf("defaults mismatch at rank %d: got %s, want %s", i, got[i].ID, want[i].ID)
		}
	}

	// (b) All-vector weighting: vec first. All-FTS weighting: kw first.
	p := DefaultSearchParams()
	p.FTSWeight, p.VecWeight = 0, 1
	if rs, err := store.SearchHybridParams(ctx, "test-proj", "goroutines", queryVec, 10, p); err != nil || len(rs) == 0 || rs[0].ID != vec {
		t.Errorf("all-vector weighting should rank vector match first (err=%v, results=%v)", err, ids(rs))
	}
	p.FTSWeight, p.VecWeight = 1, 0
	if rs, err := store.SearchHybridParams(ctx, "test-proj", "goroutines", queryVec, 10, p); err != nil || len(rs) == 0 || rs[0].ID != kw {
		t.Errorf("all-FTS weighting should rank keyword match first (err=%v, results=%v)", err, ids(rs))
	}
}

// ids extracts memory IDs for readable test failure output.
func ids(ms []Memory) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}

func TestApplyDecay_ReordersWindow(t *testing.T) {
	now := timeMustParse("2026-07-15 00:00:00")
	p := DefaultSearchParams() // DecayEnabled true

	// Within the window, decay reorders: fresh decision beats stale decision
	// at equal-ish fused scores (decay-on multiplies the window by decay).
	fresh := Memory{ID: "fresh", Category: "decision", CreatedAt: "2026-07-10 00:00:00"} // 5 days
	stale := Memory{ID: "stale", Category: "decision", CreatedAt: "2026-04-16 00:00:00"} // 90 days
	scores := map[string]float64{"fresh": 0.4, "stale": 0.5}

	got := decayRank([]Memory{stale, fresh}, scores, p, 10, now)
	if got[0].ID != "fresh" {
		t.Errorf("decay should rank fresher first, got %v", ids(got))
	}

	// DecayEnabled false ranks by base score only: stale's higher base (0.5 vs
	// 0.4) wins. (decayRank sorts by base in both modes — the fused path
	// hydrates via GetByIDs, which does not preserve order.)
	off := p
	off.DecayEnabled = false
	got = decayRank([]Memory{stale, fresh}, scores, off, 10, now)
	if got[0].ID != "stale" {
		t.Errorf("DecayEnabled=false must rank by base score, got %v", ids(got))
	}

	// Unparseable created_at treated as ancient (never spuriously wins).
	bad := Memory{ID: "bad", Category: "decision", CreatedAt: "not-a-date"}
	got = decayRank([]Memory{bad, fresh}, scores, p, 10, now)
	if got[0].ID != "fresh" {
		t.Errorf("malformed timestamp must not win; got %v", ids(got))
	}
}

// TestApplyDecay_PreservesMembership is the regression guard for the
// findability finding: truncation happens by base score ALONE, so decay
// reorders within the window but can never drop a higher-base (more relevant)
// memory below the cutoff. This is what keeps TestStalenessReport green.
func TestApplyDecay_PreservesMembership(t *testing.T) {
	now := timeMustParse("2026-07-15 00:00:00")
	p := DefaultSearchParams()

	// stale has the higher base score (0.9 vs 0.5) but is heavily decayed;
	// fresh is barely decayed but lower base. Decay must NOT let fresh displace
	// stale from a limit-1 window — relevance (base) owns membership.
	mems := []Memory{
		{ID: "stale", Category: "dependency", CreatedAt: "2026-01-01 00:00:00"}, // 195 days → floored 0.15
		{ID: "fresh", Category: "dependency", CreatedAt: "2026-07-14 00:00:00"}, // 1 day → ~0.97
	}
	scores := map[string]float64{"stale": 0.9, "fresh": 0.5}

	got := decayRank(mems, scores, p, 1, now)
	if len(got) != 1 || got[0].ID != "stale" {
		t.Errorf("decay must NOT change membership (base 0.9 > 0.5 owns the slot), got %v", ids(got))
	}

	// Same result with decay off (trivially base order).
	off := p
	off.DecayEnabled = false
	got = decayRank(mems, scores, off, 1, now)
	if len(got) != 1 || got[0].ID != "stale" {
		t.Errorf("without decay, higher base score must win, got %v", ids(got))
	}
}

func timeMustParse(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04:05", s)
	if err != nil {
		panic(err)
	}
	return t
}

// makeScoredMemory is makeMemory with an explicit importance, so a test can
// pin the pre-demote ranking instead of inheriting makeMemory's fixed 0.7.
func makeScoredMemory(t *testing.T, s *Store, content string, importance float32) string {
	t.Helper()
	id, err := s.Create(context.Background(), testProject, Memory{
		Category: "fact", Content: content, Source: "manual", Importance: importance,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return id
}

// TestProductionSearchDemotesSuperseded is the regression guard for the eval
// finding that a superseded memory could outrank its replacement in
// ghost_memory_search. The demote mechanism shipped behind
// SearchParams.SupersedeDemote but no production caller ever set it, so it was
// dead code in the live server. These are the entry points the MCP tools
// actually use — SearchHybrid (ghost_memory_search) and SearchHybridAll
// (ghost_search_all) — exercised with no query vector, the FTS-only fallback
// that runs whenever Ollama is unavailable.
func TestProductionSearchDemotesSuperseded(t *testing.T) {
	if !DefaultSearchParams().SupersedeDemote {
		t.Fatal("production default must consume supersedes links")
	}

	s := testStore(t)
	ctx := context.Background()

	// Importance is what forces the baseline ordering: without it the two
	// near-identical contents tie and the winner is decided by FTS rank on a
	// two-token difference, which is not stable enough to build an assertion
	// on. The stale memory is deliberately the higher-scoring one so the only
	// thing that can move it below its replacement is the demote pass.
	stale := makeScoredMemory(t, s, "the payments service deploys to the eu-west-1 region", 0.9)
	fresh := makeScoredMemory(t, s, "the payments service deploys to the us-east-1 region", 0.5)

	before, err := s.SearchHybrid(ctx, testProject, "payments service deploys region", nil, 10)
	if err != nil {
		t.Fatalf("SearchHybrid (before): %v", err)
	}
	if len(before) != 2 {
		t.Fatalf("expected both versions retrieved, got %d", len(before))
	}
	// The demote is only provable if the stale one wins on base score.
	if before[0].ID != stale {
		t.Fatalf("baseline must favor the stale memory, got %s first (stale=%s)", before[0].ID, stale)
	}

	if err := s.CreateLink(ctx, fresh, stale, "supersedes", 0.95, "llm"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}

	after, err := s.SearchHybrid(ctx, testProject, "payments service deploys region", nil, 10)
	if err != nil {
		t.Fatalf("SearchHybrid (after): %v", err)
	}
	if len(after) != 2 {
		t.Fatalf("demote must not drop results: got %d, want 2", len(after))
	}
	if after[0].ID != fresh || after[1].ID != stale {
		t.Errorf("superseded memory must rank below its replacement; got %s then %s (fresh=%s stale=%s)",
			after[0].ID, after[1].ID, fresh, stale)
	}

	allProjects, err := s.SearchHybridAll(ctx, "payments service deploys region", nil, 10)
	if err != nil {
		t.Fatalf("SearchHybridAll: %v", err)
	}
	if len(allProjects) != 2 {
		t.Fatalf("cross-project demote must not drop results: got %d, want 2", len(allProjects))
	}
	if allProjects[0].ID != fresh {
		t.Errorf("ghost_search_all must demote superseded too; got %s first", allProjects[0].ID)
	}
}
