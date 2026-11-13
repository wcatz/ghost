package memory

import (
	"testing"
)

// The tests here cover the query side of the identity rule: who may hand out a
// stored vector to be compared with something, and who counts as covered when
// an operator asks how healthy the store is. seedIdentityRows (vector_identity_test.go)
// supplies one current-identity and one foreign vector that both point the same
// way, so only the identity can be what separates them.

// TestGetEmbeddingRefusesForeignVector: the linker and `ghost supersede` both
// use a stored vector as the query vector for SearchVector. Handing back a
// vector from another space would compare two spaces and turn the resulting
// number into a `related` edge or a `supersedes` candidate.
func TestGetEmbeddingRefusesForeignVector(t *testing.T) {
	store, ctx := setupTestStore(t)
	store.SetEmbeddingIdentity(identityCurrent)

	current, stale := seedIdentityRows(t, store, ctx)

	got, err := store.GetEmbedding(ctx, current)
	if err != nil {
		t.Fatalf("GetEmbedding(current): %v", err)
	}
	if len(got) != 3 {
		t.Errorf("GetEmbedding(current) = %v, want the stored 3-dim vector", got)
	}

	got, err = store.GetEmbedding(ctx, stale)
	if err != nil {
		t.Fatalf("GetEmbedding(foreign): %v", err)
	}
	if got != nil {
		t.Errorf("GetEmbedding(foreign) = %v, want nil: a vector from another space is not comparable with anything here", got)
	}
}

// TestUnscannedEmbeddedMemoryIDsSkipsForeignVectors: the linking queue must not
// hand the worker a vector it cannot compare, and must not spend that memory's
// single scan slot on the attempt — the link has to survive until the row is
// re-embedded.
func TestUnscannedEmbeddedMemoryIDsSkipsForeignVectors(t *testing.T) {
	store, ctx := setupTestStore(t)
	store.SetEmbeddingIdentity(identityCurrent)

	current, stale := seedIdentityRows(t, store, ctx)

	ids, err := store.UnscannedEmbeddedMemoryIDs(ctx, "test-proj", 10)
	if err != nil {
		t.Fatalf("UnscannedEmbeddedMemoryIDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != current {
		t.Fatalf("UnscannedEmbeddedMemoryIDs = %v, want only %s: the foreign vector %s must stay queued for after its re-embed", ids, current, stale)
	}
}

// TestEmbeddingStatsCountsOnlyUsableVectors: `ghost mcp status` and
// `ghost_health` read this, so a row awaiting re-embed must not be counted as
// embedded. Counting it is what let a store mid-re-embed report full coverage
// while the vector leg returned nothing.
func TestEmbeddingStatsCountsOnlyUsableVectors(t *testing.T) {
	store, ctx := setupTestStore(t)
	store.SetEmbeddingIdentity(identityCurrent)

	_, _ = seedIdentityRows(t, store, ctx)

	embedded, stale, total, err := store.EmbeddingStats(ctx)
	if err != nil {
		t.Fatalf("EmbeddingStats: %v", err)
	}
	if embedded != 1 || total != 2 {
		t.Fatalf("EmbeddingStats = %d/%d, want 1/2: only the current-identity row is searchable", embedded, total)
	}
	// The stale row is reported rather than silently folded into the gap: it
	// is the half of "awaiting re-embed" the re-embed worker already owns.
	if stale != 1 {
		t.Errorf("EmbeddingStats stale = %d, want 1: the retired-identity row must be reported as stale", stale)
	}
}

// TestEmbeddingCosinesExcludesOtherIdentity: EmbeddingCosines is how a caller
// scores a result the vector leg never returned a score for — a keyword-reserved
// row whose cosine put it below every fetched vector list. Scoring a row from a
// retired space would return a number that looks like a confidence and means
// nothing, which is the whole hazard GetEmbedding refuses to take. Same dimensions
// as the query on purpose, so the width check cannot be what drops it.
func TestEmbeddingCosinesExcludesOtherIdentity(t *testing.T) {
	store, ctx := setupTestStore(t)
	store.SetEmbeddingIdentity(identityCurrent)

	current, stale := seedIdentityRows(t, store, ctx)

	cosines, err := store.EmbeddingCosines(ctx, []string{current, stale}, []float32{1, 0, 0})
	if err != nil {
		t.Fatalf("EmbeddingCosines: %v", err)
	}
	if got, ok := cosines[current]; !ok || got < 0.99 {
		t.Errorf("cosine for the current-identity row = %v (present=%v), want ~1", got, ok)
	}
	if got, ok := cosines[stale]; ok {
		t.Errorf("cosine for the foreign-identity row = %v, want absent: a vector from another space is not comparable with a query in this one", got)
	}
}

// TestEmbeddingCosinesSkipsWrongWidth: a row whose stored width differs from the
// query's was written by a model that does not produce queryVec, so it has no
// comparable score either — absent, not zero.
func TestEmbeddingCosinesSkipsWrongWidth(t *testing.T) {
	store, ctx := setupTestStore(t)

	id := createTestMemory(t, store, ctx, "vector from a wider model")
	if err := store.StoreEmbedding(ctx, id, []float32{1, 0, 0, 0}, identityCurrent); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}

	cosines, err := store.EmbeddingCosines(ctx, []string{id}, []float32{1, 0, 0})
	if err != nil {
		t.Fatalf("EmbeddingCosines: %v", err)
	}
	if got, ok := cosines[id]; ok {
		t.Errorf("cosine for a wrong-width row = %v, want absent", got)
	}
}
