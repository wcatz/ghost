package memory

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
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

// TestEmbeddingStatsIdentityUnsetCountsEveryRow: the unconfigured store keeps
// the plain row count, which is what an install with no embedding model wants.
func TestEmbeddingStatsIdentityUnsetCountsEveryRow(t *testing.T) {
	store, ctx := setupTestStore(t)

	_, _ = seedIdentityRows(t, store, ctx)

	embedded, stale, total, err := store.EmbeddingStats(ctx)
	if err != nil {
		t.Fatalf("EmbeddingStats: %v", err)
	}
	if embedded != 2 || total != 2 {
		t.Fatalf("EmbeddingStats with no configured identity = %d/%d, want 2/2", embedded, total)
	}
	if stale != 0 {
		t.Errorf("EmbeddingStats stale with no configured identity = %d, want 0: nothing is retired when nothing is configured", stale)
	}
}

// TestEmbeddingStatsSplitsStaleFromUnembedded: the gap the coverage line
// reports has two halves that mean different things — a stale row has a vector
// under a retired identity, so the re-embed worker has the work queued; an
// unembedded row has never had a vector, so nothing has run for it. `ghost mcp
// status` and `ghost_health` print both, so the arithmetic has to hold: stale
// is a subset of the gap, never part of the covered count.
func TestEmbeddingStatsSplitsStaleFromUnembedded(t *testing.T) {
	store, ctx := setupTestStore(t)
	store.SetEmbeddingIdentity(identityCurrent)

	_, _ = seedIdentityRows(t, store, ctx)
	createTestMemory(t, store, ctx, "memory that never had a vector")

	embedded, stale, total, err := store.EmbeddingStats(ctx)
	if err != nil {
		t.Fatalf("EmbeddingStats: %v", err)
	}
	if embedded != 1 || stale != 1 || total != 3 {
		t.Fatalf("EmbeddingStats = embedded %d, stale %d, total %d, want 1, 1, 3", embedded, stale, total)
	}
	if unembedded := total - embedded - stale; unembedded != 1 {
		t.Errorf("unembedded = %d, want 1: the gap minus the stale rows is what has never been embedded", unembedded)
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

// TestEmbeddingCosinesIdentityUnsetScoresEveryRow: with no identity configured
// the lookup behaves as before — the bench harness and tests run on such a store,
// and a store nobody configured an embedding model for has no spaces to confuse.
func TestEmbeddingCosinesIdentityUnsetScoresEveryRow(t *testing.T) {
	store, ctx := setupTestStore(t)

	_, stale := seedIdentityRows(t, store, ctx)

	cosines, err := store.EmbeddingCosines(ctx, []string{stale}, []float32{1, 0, 0})
	if err != nil {
		t.Fatalf("EmbeddingCosines: %v", err)
	}
	if got, ok := cosines[stale]; !ok || got < 0.99 {
		t.Errorf("cosine with no configured identity = %v (present=%v), want ~1", got, ok)
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

// TestEmbeddingCosinesWarnsOncePerIdentity: the cosine lookup runs once per
// query — the bench harness calls it 244 times in one `ghost bench` — so a
// warning logged on every call reports the same reconfiguration a line at a
// time for as long as the re-embed runs, which buries the state it exists to
// report. The warning still has to fire (a reconfiguration is exactly when the
// operator is watching), just once, through the same per-retired-identity gate
// searchVector's warning uses.
func TestEmbeddingCosinesWarnsOncePerIdentity(t *testing.T) {
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
	id := createTestMemory(t, store, ctx, "memory from the retired space")
	if err := store.StoreEmbedding(ctx, id, []float32{1, 0, 0}, identityStale); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}

	// Two calls, the gate's own claim on the first: one line, not two.
	for i := 0; i < 2; i++ {
		if _, err := store.EmbeddingCosines(ctx, []string{id}, []float32{1, 0, 0}); err != nil {
			t.Fatalf("EmbeddingCosines #%d: %v", i+1, err)
		}
	}
	if got := strings.Count(buf.String(), "another vector space"); got != 1 {
		t.Errorf("cosine lookup logged the foreign-vector warning %d times across 2 calls, want 1: one line per retired identity, whatever the call rate", got)
	}
}

// TestEmbeddingCosinesSharesTheSearchWarningGate: routing through the gate
// rather than a private flag is what keeps one process to one line for a
// retirement, whichever path noticed it first. A search that has already
// reported a retired identity must leave the cosine lookup nothing to say —
// the two are the same diagnosis, and the operator needs it once.
func TestEmbeddingCosinesSharesTheSearchWarningGate(t *testing.T) {
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
	id := createTestMemory(t, store, ctx, "memory from the retired space")
	if err := store.StoreEmbedding(ctx, id, []float32{1, 0, 0}, identityStale); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}

	if _, err := store.SearchVector(ctx, "test-proj", []float32{1, 0, 0}, 10); err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	if _, err := store.EmbeddingCosines(ctx, []string{id}, []float32{1, 0, 0}); err != nil {
		t.Fatalf("EmbeddingCosines: %v", err)
	}
	if got := strings.Count(buf.String(), "another vector space"); got != 1 {
		t.Errorf("search + cosine lookup logged %d warnings for one retired identity, want 1: both paths must spend the same gate, or a query costs a second line", got)
	}
}
