package memory

import (
	"sync"
	"testing"
)

// TestStoreEmbeddingAndMarkLinkScannedCannotReArmTheSlot is #1258.
//
// StoreEmbedding rewrites a row into a new vector space and, when the identity
// changed, retires the link scan the memory earned in the old space so the
// linker re-scans it (see TestReembedUnderNewIdentityRequeuesForLinking). The
// clear and the upsert have to happen as one transaction. Run in two, a
// MarkLinkScanned for the OLD vector can commit in between: the row still holds
// the retired bytes at that instant, so the mark's byte check passes, the slot
// is written back after the clear, and the upsert then installs the new model.
// The row is left holding a scan slot computed in the retired space —
// UnscannedEmbeddedMemoryIDs (e.model = ? AND no slot) never returns it, so the
// memory is never link-scanned in the new space.
//
// The loop is the reproduction the issue gives, kept because the window is a
// real interleaving rather than a value a single call can carry: with the two
// writes in one transaction, no ordering of the concurrent mark can leave the
// slot armed. A mark that commits before the store is cleared by the store's own
// DELETE; a mark that commits after the upsert is refused by its byte check; and
// a mark landing between them is what the one transaction removes.
//
// TestVectorEpochInvalidatesOnEveryVectorAffectingWrite asserts the clear and
// the upsert separately and cannot catch this: it never runs a mark while the
// re-embed is in flight.
func TestStoreEmbeddingAndMarkLinkScannedCannotReArmTheSlot(t *testing.T) {
	store, ctx := setupTestStore(t)
	id := createTestMemory(t, store, ctx, "a memory that two vector spaces meet on")

	// Distinct bytes, so the mark's byte check cannot match the new vector and
	// the only way a slot survives is the retired-space re-arm under test.
	vOld := []float32{1, 0, 0, 0, 0, 0, 0, 0}
	vNew := []float32{0, 1, 0, 0, 0, 0, 0, 0}
	const (
		oldModel = "m1:8"
		newModel = "m2:8"
	)

	// Bounded: the loop only exists to meet the interleaving, and a fixed count
	// keeps the fixed implementation cheap. A single value is not enough because
	// the race is scheduling-dependent.
	const iterations = 500
	for i := 0; i < iterations; i++ {
		if err := store.StoreEmbedding(ctx, id, vOld, oldModel); err != nil {
			t.Fatalf("iteration %d: StoreEmbedding(old): %v", i, err)
		}
		if _, err := store.db.ExecContext(ctx, `DELETE FROM link_scans WHERE memory_id = ?`, id); err != nil {
			t.Fatalf("iteration %d: reset the scan slot: %v", i, err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := store.StoreEmbedding(ctx, id, vNew, newModel); err != nil {
				t.Errorf("iteration %d: StoreEmbedding(new): %v", i, err)
			}
		}()
		go func() {
			defer wg.Done()
			if err := store.MarkLinkScanned(ctx, id, vOld); err != nil {
				t.Errorf("iteration %d: MarkLinkScanned: %v", i, err)
			}
		}()
		wg.Wait()

		var model string
		if err := store.db.QueryRowContext(ctx,
			`SELECT model FROM memory_embeddings WHERE memory_id = ?`, id).Scan(&model); err != nil {
			t.Fatalf("iteration %d: read the stored model: %v", i, err)
		}
		if model != newModel {
			t.Fatalf("iteration %d: stored model = %q, want %q: the re-embed did not win", i, model, newModel)
		}
		if !linkScanQueued(t, store, ctx, "test-proj", id) {
			t.Fatalf("iteration %d: the memory holds a scan slot in the retired space (%s); "+
				"it must stay queued for a scan in %s", i, oldModel, newModel)
		}
	}
}
