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

// TestGetEmbeddingIdentityUnsetReturnsEveryVector: with no identity configured
// the blob comes back as before — the bench harness and tests rely on it.
func TestGetEmbeddingIdentityUnsetReturnsEveryVector(t *testing.T) {
	store, ctx := setupTestStore(t)

	_, stale := seedIdentityRows(t, store, ctx)

	got, err := store.GetEmbedding(ctx, stale)
	if err != nil {
		t.Fatalf("GetEmbedding: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("GetEmbedding with no configured identity = %v, want the stored vector", got)
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

	embedded, total, err := store.EmbeddingStats(ctx)
	if err != nil {
		t.Fatalf("EmbeddingStats: %v", err)
	}
	if embedded != 1 || total != 2 {
		t.Fatalf("EmbeddingStats = %d/%d, want 1/2: only the current-identity row is searchable", embedded, total)
	}
}

// TestEmbeddingStatsIdentityUnsetCountsEveryRow: the unconfigured store keeps
// the plain row count, which is what an install with no embedding model wants.
func TestEmbeddingStatsIdentityUnsetCountsEveryRow(t *testing.T) {
	store, ctx := setupTestStore(t)

	_, _ = seedIdentityRows(t, store, ctx)

	embedded, total, err := store.EmbeddingStats(ctx)
	if err != nil {
		t.Fatalf("EmbeddingStats: %v", err)
	}
	if embedded != 2 || total != 2 {
		t.Fatalf("EmbeddingStats with no configured identity = %d/%d, want 2/2", embedded, total)
	}
}

// TestExplainSearchScoresOnlyCurrentIdentityVectors: explain exists to
// describe the ranking the search returned, and it runs on a derived trace
// store — so any knob the trace does not inherit is a knob the trace reports
// differently from the real search. The query shares no words with either
// memory, so the window can only be the vector leg's.
func TestExplainSearchScoresOnlyCurrentIdentityVectors(t *testing.T) {
	store, ctx := setupTestStore(t)
	store.SetEmbeddingIdentity(identityCurrent)

	current, stale := seedIdentityRows(t, store, ctx)

	ex, err := store.ExplainSearch(ctx, "test-proj", "zzyzx unrelated query", []float32{1, 0, 0}, 10)
	if err != nil {
		t.Fatalf("ExplainSearch: %v", err)
	}

	included := map[string]ExplainRow{}
	for _, row := range ex.Rows {
		if row.Included {
			included[row.ID] = row
		}
		if row.ID == stale && row.VectorRank != -1 {
			t.Errorf("explain gave the foreign-identity memory %s vector_rank=%d (score %v): the trace store is not inheriting the configured identity",
				stale, row.VectorRank, row.VectorScore)
		}
	}
	row, ok := included[current]
	if !ok {
		t.Fatalf("explain included %v, want the current-identity memory %s", included, current)
	}
	// VectorRank is the 0-based position in the vector leg, so the sole
	// candidate is 0; -1 would mean the leg never saw it.
	if row.VectorRank != 0 {
		t.Errorf("current-identity memory has vector_rank=%d, want 0: it should be the only vector candidate", row.VectorRank)
	}
	if len(included) != 1 {
		t.Errorf("explain included %d memories (%v), want 1", len(included), included)
	}
}

// TestExplainSearchStillFindsForeignMemoryByKeyword: retiring a vector does not
// retire the memory. Its text is still keyword-searchable, and explain must
// report that as a keyword-only hit with no vector rank, not as an absence.
func TestExplainSearchStillFindsForeignMemoryByKeyword(t *testing.T) {
	store, ctx := setupTestStore(t)
	store.SetEmbeddingIdentity(identityCurrent)

	_, stale := seedIdentityRows(t, store, ctx)

	ex, err := store.ExplainSearch(ctx, "test-proj", "old space", []float32{1, 0, 0}, 10)
	if err != nil {
		t.Fatalf("ExplainSearch: %v", err)
	}

	for _, row := range ex.Rows {
		if row.ID != stale {
			continue
		}
		if !row.Included {
			t.Fatalf("explain excluded the foreign-identity memory %s entirely; only its vector is retired, its text is still searchable", stale)
		}
		if row.VectorRank != -1 {
			t.Errorf("keyword-only hit has vector_rank=%d, want -1: a vector from another space may not be scored", row.VectorRank)
		}
		return
	}
	t.Fatalf("explain did not return the memory %s at all for a keyword that matches it", stale)
}
