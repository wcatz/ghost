package memory

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// The tests here cover what a re-embed owes the linking and health paths now
// that a stored vector records the space that produced it (#642): the link
// scan slot a memory earned in the old space, and the warning a pending
// re-embed produces on every search.

// linkScanQueued reports whether the memory is back in the linking worker's
// queue — i.e. link_scans holds no row for it.
func linkScanQueued(t *testing.T, store *Store, ctx context.Context, projectID, memoryID string) bool {
	t.Helper()
	ids, err := store.UnscannedEmbeddedMemoryIDs(ctx, projectID, 50)
	if err != nil {
		t.Fatalf("UnscannedEmbeddedMemoryIDs: %v", err)
	}
	for _, id := range ids {
		if id == memoryID {
			return true
		}
	}
	return false
}

// TestReembedUnderNewIdentityRequeuesForLinking: link_scans records that a
// memory's neighbours were compared in ONE vector space. The embedding
// worker rewriting the row under a new identity retires that comparison, so
// the slot has to go with it — otherwise the memory is never scanned again,
// the links it keeps were built from a space nothing searches any more, and
// they feed `ghost supersede` and the Obsidian graph forever.
func TestReembedUnderNewIdentityRequeuesForLinking(t *testing.T) {
	store, ctx := setupTestStore(t)
	store.SetEmbeddingIdentity(identityCurrent)

	id := createTestMemory(t, store, ctx, "memory whose space changes")
	if err := store.StoreEmbedding(ctx, id, []float32{1, 0, 0}, identityStale); err != nil {
		t.Fatalf("StoreEmbedding(stale): %v", err)
	}
	if err := store.MarkLinkScanned(ctx, id); err != nil {
		t.Fatalf("MarkLinkScanned: %v", err)
	}
	if linkScanQueued(t, store, ctx, "test-proj", id) {
		t.Fatal("memory is queued before the re-embed; the fixture must start scanned")
	}

	// What the embedding worker does next: rewrite the row in the configured
	// space.
	if err := store.StoreEmbedding(ctx, id, []float32{1, 0, 0}, identityCurrent); err != nil {
		t.Fatalf("StoreEmbedding(current): %v", err)
	}

	if !linkScanQueued(t, store, ctx, "test-proj", id) {
		t.Errorf("memory %s was not re-queued after its embedding was rewritten under a new identity: its links stay those of the retired space", id)
	}
}

// TestReembedInSameIdentityKeepsLinkScan is the other half of the rule: the
// slot is only worth clearing when the vector space changed. A rewrite in the
// same space (the worker topping up a row, the bench harness) leaves a valid
// scan behind, and clearing it there would put every memory back in the
// queue on every sweep.
func TestReembedInSameIdentityKeepsLinkScan(t *testing.T) {
	store, ctx := setupTestStore(t)
	store.SetEmbeddingIdentity(identityCurrent)

	id := createTestMemory(t, store, ctx, "memory whose space holds")
	if err := store.StoreEmbedding(ctx, id, []float32{1, 0, 0}, identityCurrent); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}
	if err := store.MarkLinkScanned(ctx, id); err != nil {
		t.Fatalf("MarkLinkScanned: %v", err)
	}

	if err := store.StoreEmbedding(ctx, id, []float32{1, 0, 0}, identityCurrent); err != nil {
		t.Fatalf("StoreEmbedding again: %v", err)
	}

	if linkScanQueued(t, store, ctx, "test-proj", id) {
		t.Errorf("memory %s was re-queued although its vector space did not change", id)
	}
}

// TestForeignVectorWarningLoggedOnce: during a re-embed every search skips
// foreign rows, so a per-search warning is one line per query for as long as
// the rewrite runs — the log noise drowns the very state it reports. The
// warning still has to fire (a reconfiguration is exactly when the operator
// is watching), just once: the gate is claimed by the first search and is
// shared with explain's trace store, so one process logs one line.
func TestForeignVectorWarningLoggedOnce(t *testing.T) {
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

	// The gate belongs to the store built above, so nothing else in this
	// package's tests can have spent it: what follows is this store's own
	// three searches.
	for i := 0; i < 3; i++ {
		if _, err := store.SearchVector(ctx, "test-proj", []float32{1, 0, 0}, 10); err != nil {
			t.Fatalf("SearchVector #%d: %v", i+1, err)
		}
	}

	if got := strings.Count(buf.String(), "another vector space"); got != 1 {
		t.Errorf("foreign-vector warning logged %d times across 3 searches, want 1: the re-embed window lasts many searches, the warning must not", got)
	}
}

// TestForeignVectorWarningRetiresPerIdentity: the gate that silences the
// repeat warnings must be keyed on the retired identity, not the process. A
// long-lived MCP server can reconfigure twice in one session (a second model,
// dimension or prefix change), and the second retirement is a genuinely new
// diagnosis with its own stored_identity — a process-wide gate would swallow
// it and log nothing while the first retirement's rows are long gone. Each
// retired identity warns once; repeats within one identity stay quiet.
func TestForeignVectorWarningRetiresPerIdentity(t *testing.T) {
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
	first := createTestMemory(t, store, ctx, "memory from the first retired space")
	if err := store.StoreEmbedding(ctx, first, []float32{1, 0, 0}, identityStale); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := store.SearchVector(ctx, "test-proj", []float32{1, 0, 0}, 10); err != nil {
			t.Fatalf("SearchVector #%d: %v", i+1, err)
		}
	}
	if got := strings.Count(buf.String(), "another vector space"); got != 1 {
		t.Fatalf("after the first retirement the warning logged %d times across 2 searches, want 1", got)
	}

	// The first reconfiguration completes (its rows are rewritten in the
	// configured space) and a second, different identity retires in the same
	// process — a model change after a prefix change, say.
	const identitySecond = "another-model:384+prefix"
	if err := store.StoreEmbedding(ctx, first, []float32{1, 0, 0}, identityCurrent); err != nil {
		t.Fatalf("StoreEmbedding(current): %v", err)
	}
	second := createTestMemory(t, store, ctx, "memory from the second retired space")
	if err := store.StoreEmbedding(ctx, second, []float32{0, 1, 0}, identitySecond); err != nil {
		t.Fatalf("StoreEmbedding(second identity): %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := store.SearchVector(ctx, "test-proj", []float32{1, 0, 0}, 10); err != nil {
			t.Fatalf("SearchVector (second identity) #%d: %v", i+1, err)
		}
	}
	if got := strings.Count(buf.String(), "another vector space"); got != 2 {
		t.Errorf("after a second retired identity the warning logged %d times total, want 2: the gate is per identity, so a new retirement in the same process must warn again", got)
	}
}

// TestForeignVectorWarningNamesOverlappingRetirements: the realistic
// reconfiguration is OVERLAPPING — the operator changes the model, the re-embed
// has not finished (50 memories per project per sweep), and then the
// dimensions or prefix change again. Both retired identities' rows are still in
// the store at once, and the scan has no ORDER BY, so whichever identity the
// first foreign row happens to carry must not absorb the other: each retired
// identity gets its own line with its own skipped count, in the same search if
// needed.
func TestForeignVectorWarningNamesOverlappingRetirements(t *testing.T) {
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
	// Two retirements coexist: neither has been rewritten yet.
	const identityOlder = "oldest-model:384+prefix"
	first := createTestMemory(t, store, ctx, "memory from the older retired space")
	if err := store.StoreEmbedding(ctx, first, []float32{1, 0, 0}, identityOlder); err != nil {
		t.Fatalf("StoreEmbedding(older): %v", err)
	}
	second := createTestMemory(t, store, ctx, "memory from the newer retired space")
	if err := store.StoreEmbedding(ctx, second, []float32{0, 1, 0}, identityStale); err != nil {
		t.Fatalf("StoreEmbedding(newer): %v", err)
	}

	for i := 0; i < 2; i++ {
		if _, err := store.SearchVector(ctx, "test-proj", []float32{1, 0, 0}, 10); err != nil {
			t.Fatalf("SearchVector #%d: %v", i+1, err)
		}
	}

	logged := buf.String()
	if got := strings.Count(logged, "another vector space"); got != 2 {
		t.Errorf("two overlapping retired identities logged %d warning lines, want 2 (one per identity, repeats suppressed)", got)
	}
	for _, id := range []string{identityOlder, identityStale} {
		want := "skipped=1 usable=0 configured_identity=" + identityCurrent + " stored_identity=" + id
		if !strings.Contains(logged, want) {
			t.Errorf("no warning line reports its own identity's skip count for retired identity %q (want substring %q); an overlapping retirement was absorbed by the older one. Log was:\n%s", id, want, logged)
		}
	}
}
