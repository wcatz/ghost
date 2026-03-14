package memory

import (
	"context"
	"math"
	"testing"
)

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
	t.Cleanup(func() { db.Close() })

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
