package memory

import (
	"sync"
	"testing"
)

// TestStoreEmbeddingAndMarkLinkScannedCannotReArmTheSlot is #1258. StoreEmbedding
// retires the link scan a memory earned in the old vector space in the same
// transaction as the upsert; in two, a MarkLinkScanned for the OLD vector could
// commit between them, pass its byte check against the not-yet-replaced row and
// leave a scan slot computed in the retired space, so the memory is never scanned
// in the new one. The loop meets that scheduling-dependent interleaving (no single
// call can carry it); with one transaction no ordering can leave the slot armed.
func TestStoreEmbeddingAndMarkLinkScannedCannotReArmTheSlot(t *testing.T) {
	store, ctx := setupTestStore(t)
	id := createTestMemory(t, store, ctx, "a memory that two vector spaces meet on")

	vOld := []float32{1, 0, 0, 0, 0, 0, 0, 0}
	vNew := []float32{0, 1, 0, 0, 0, 0, 0, 0}
	const (
		oldModel = "m1:8"
		newModel = "m2:8"
	)

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
