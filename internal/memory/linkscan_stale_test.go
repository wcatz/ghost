package memory

import "testing"

// TestMarkLinkScannedRecordsOnlyTheCurrentVector: a link scan compares ONE
// stored vector with its neighbours, so recording it is only true while that
// vector is still the memory's embedding. An edit landing mid-scan deletes the
// vector and the scan slot together; the scan finishing afterwards must not
// write the slot back, whichever of the scan and the re-embed of the new text
// lands first, or the new text is never scanned (UnscannedEmbeddedMemoryIDs
// skips it for good and only the old text's edges remain).
func TestMarkLinkScannedRecordsOnlyTheCurrentVector(t *testing.T) {
	oldVec := []float32{1, 0, 0}
	newVec := []float32{0, 1, 0}
	newText := "rewritten text the old vector was never computed from"

	t.Run("scan finishes before the re-embed", func(t *testing.T) {
		store, ctx := setupTestStore(t)
		id := createTestMemory(t, store, ctx, "original text")
		if err := store.StoreEmbedding(ctx, id, oldVec, "test"); err != nil {
			t.Fatalf("StoreEmbedding(old): %v", err)
		}
		scanned, err := store.GetEmbedding(ctx, id)
		if err != nil {
			t.Fatalf("GetEmbedding: %v", err)
		}
		if err := store.UpdateMemory(ctx, "test-proj", id, &newText, nil, nil, nil); err != nil {
			t.Fatalf("UpdateMemory: %v", err)
		}
		if err := store.MarkLinkScanned(ctx, id, scanned); err != nil {
			t.Fatalf("MarkLinkScanned: %v", err)
		}
		if err := store.StoreEmbedding(ctx, id, newVec, "test"); err != nil {
			t.Fatalf("StoreEmbedding(new): %v", err)
		}
		if !linkScanQueued(t, store, ctx, "test-proj", id) {
			t.Errorf("memory %s is not queued for a scan of its new text: the scan of its old vector was recorded", id)
		}
	})

	t.Run("re-embed lands before the scan finishes", func(t *testing.T) {
		store, ctx := setupTestStore(t)
		id := createTestMemory(t, store, ctx, "original text")
		if err := store.StoreEmbedding(ctx, id, oldVec, "test"); err != nil {
			t.Fatalf("StoreEmbedding(old): %v", err)
		}
		scanned, err := store.GetEmbedding(ctx, id)
		if err != nil {
			t.Fatalf("GetEmbedding: %v", err)
		}
		if err := store.UpdateMemory(ctx, "test-proj", id, &newText, nil, nil, nil); err != nil {
			t.Fatalf("UpdateMemory: %v", err)
		}
		if err := store.StoreEmbedding(ctx, id, newVec, "test"); err != nil {
			t.Fatalf("StoreEmbedding(new): %v", err)
		}
		if err := store.MarkLinkScanned(ctx, id, scanned); err != nil {
			t.Fatalf("MarkLinkScanned: %v", err)
		}
		if !linkScanQueued(t, store, ctx, "test-proj", id) {
			t.Errorf("memory %s is not queued for a scan of its new text: the scan of its old vector was recorded", id)
		}
	})

	t.Run("a scan of the current vector is recorded", func(t *testing.T) {
		store, ctx := setupTestStore(t)
		id := createTestMemory(t, store, ctx, "original text")
		// Not a unit vector, so a read that normalised or otherwise
		// transformed the stored bytes could not match them by accident.
		if err := store.StoreEmbedding(ctx, id, []float32{0.3, -1.7, 2.25}, "test"); err != nil {
			t.Fatalf("StoreEmbedding: %v", err)
		}
		scanned, err := store.GetEmbedding(ctx, id)
		if err != nil {
			t.Fatalf("GetEmbedding: %v", err)
		}
		if err := store.MarkLinkScanned(ctx, id, scanned); err != nil {
			t.Fatalf("MarkLinkScanned: %v", err)
		}
		if linkScanQueued(t, store, ctx, "test-proj", id) {
			t.Errorf("memory %s is still queued after a scan of its current vector", id)
		}
	})
}
