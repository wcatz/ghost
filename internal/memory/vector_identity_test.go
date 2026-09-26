package memory

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// The identity tests all use vectors of the SAME dimension, so nothing but the
// recorded identity can explain a row being skipped or re-queued.

const (
	identityCurrent = "nomic-embed-text:v1.5:768+prefix"
	identityStale   = "nomic-embed-text:v1.5:768"
)

// seedIdentityRows creates two memories and embeds one with the current
// identity and one with a stale one, both 3-dimensional. Returns their IDs in
// that order.
//
// Both vectors point the same way, so the stale row is a POSITIVE match for the
// query the tests use: with only the cosine floor standing between them and the
// identity check, the stale row would reach the results — which is what lets
// these tests tell "filtered by identity" from "unlucky geometry".
func seedIdentityRows(t *testing.T, store *Store, ctx context.Context) (current, stale string) {
	t.Helper()
	current = createTestMemory(t, store, ctx, "vector in the configured space")
	stale = createTestMemory(t, store, ctx, "vector from the old space")
	if err := store.StoreEmbedding(ctx, current, []float32{1, 0, 0}, identityCurrent); err != nil {
		t.Fatalf("StoreEmbedding(current): %v", err)
	}
	if err := store.StoreEmbedding(ctx, stale, []float32{0.9, 0.1, 0}, identityStale); err != nil {
		t.Fatalf("StoreEmbedding(stale): %v", err)
	}
	return current, stale
}

// TestSearchVectorExcludesOtherIdentity: a vector stamped with a different
// identity is not comparable to a query embedded in the configured space, so
// it must not reach the vector leg. Same dimensions as the query on purpose —
// the dimension check cannot be what drops it.
func TestSearchVectorExcludesOtherIdentity(t *testing.T) {
	store, ctx := setupTestStore(t)
	store.SetEmbeddingIdentity(identityCurrent)

	current, stale := seedIdentityRows(t, store, ctx)

	results, err := store.SearchVector(ctx, "test-proj", []float32{1, 0, 0}, 10)
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	if len(results) != 1 || results[0].MemoryID != current {
		t.Fatalf("SearchVector = %+v, want only the current-identity memory %s (stale %s must be excluded)", results, current, stale)
	}
}

// TestSearchVectorAllExcludesOtherIdentity is TestSearchVectorExcludesOtherIdentity
// for the cross-project leg (ghost_search_all), which scans the same table
// through a separate code path.
func TestSearchVectorAllExcludesOtherIdentity(t *testing.T) {
	store, ctx := setupTestStore(t)
	store.SetEmbeddingIdentity(identityCurrent)

	current, stale := seedIdentityRows(t, store, ctx)

	results, err := store.SearchVectorAll(ctx, []float32{1, 0, 0}, 10)
	if err != nil {
		t.Fatalf("SearchVectorAll: %v", err)
	}
	if len(results) != 1 || results[0].MemoryID != current {
		t.Fatalf("SearchVectorAll = %+v, want only the current-identity memory %s (stale %s must be excluded)", results, current, stale)
	}
}

// TestSearchVectorIdentityUnsetKeepsEveryRow: the filter is opt-in, so a store
// with no configured identity (the bench harness, tests) still searches every
// stored vector.
func TestSearchVectorIdentityUnsetKeepsEveryRow(t *testing.T) {
	store, ctx := setupTestStore(t)

	seedIdentityRows(t, store, ctx)

	results, err := store.SearchVector(ctx, "test-proj", []float32{1, 0, 0}, 10)
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("SearchVector with no configured identity returned %d results, want 2 (the unfiltered historical behaviour)", len(results))
	}
}

// TestSearchVectorOtherIdentityIsSurfaced: skipping a foreign vector space is
// a configuration state the operator has to be able to see, the same way a
// dimension mismatch already is.
func TestSearchVectorOtherIdentityIsSurfaced(t *testing.T) {
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var buf bytes.Buffer
	store := NewStore(db, slog.New(slog.NewTextHandler(&buf, nil)))
	store.SetEmbeddingIdentity(identityCurrent)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "test-proj", "/test", "test"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	id := createTestMemory(t, store, ctx, "some memory")
	if err := store.StoreEmbedding(ctx, id, []float32{1, 0, 0}, identityStale); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}

	results, err := store.SearchVector(ctx, "test-proj", []float32{1, 0, 0}, 10)
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results for a foreign-identity vector, got %d", len(results))
	}

	out := buf.String()
	if !strings.Contains(out, "identity") || !strings.Contains(out, identityStale) {
		t.Fatalf("excluded embeddings were not surfaced: %q", out)
	}
}

// TestUnembeddedMemoryIDsRequeuesOtherIdentity: a foreign-identity vector is
// treated as missing, so the embedding worker picks the row up again and
// rewrites it in the configured space.
func TestUnembeddedMemoryIDsRequeuesOtherIdentity(t *testing.T) {
	store, ctx := setupTestStore(t)

	current, stale := seedIdentityRows(t, store, ctx)
	never := createTestMemory(t, store, ctx, "no vector at all")

	ids, err := store.UnembeddedMemoryIDs(ctx, "test-proj", identityCurrent, 10)
	if err != nil {
		t.Fatalf("UnembeddedMemoryIDs: %v", err)
	}
	got := strings.Join(ids, ",")
	if !strings.Contains(got, stale) {
		t.Errorf("UnembeddedMemoryIDs = %v, want it to include the stale-identity memory %s", ids, stale)
	}
	if !strings.Contains(got, never) {
		t.Errorf("UnembeddedMemoryIDs = %v, want it to include the never-embedded memory %s", ids, never)
	}
	if strings.Contains(got, current) {
		t.Errorf("UnembeddedMemoryIDs = %v, must not include %s: its vector already matches the configured identity", ids, current)
	}
}

// TestUnembeddedMemoryIDsEmptyIdentityIsExistenceOnly: an empty identity keeps
// the historical meaning — the row only has to have a vector, whatever wrote
// it.
func TestUnembeddedMemoryIDsEmptyIdentityIsExistenceOnly(t *testing.T) {
	store, ctx := setupTestStore(t)

	_, _ = seedIdentityRows(t, store, ctx)
	never := createTestMemory(t, store, ctx, "no vector at all")

	ids, err := store.UnembeddedMemoryIDs(ctx, "test-proj", "", 10)
	if err != nil {
		t.Fatalf("UnembeddedMemoryIDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != never {
		t.Fatalf("UnembeddedMemoryIDs(identity=\"\") = %v, want only the never-embedded memory %s", ids, never)
	}
}
