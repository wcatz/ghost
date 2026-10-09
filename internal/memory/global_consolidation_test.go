package memory

import (
	"context"
	"testing"
	"time"
)

// TestConsolidateGlobal_DryRun tests that dry-run lists clusters without writing.
func TestConsolidateGlobal_DryRun(t *testing.T) {
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	store := NewStore(db, nil)
	ctx := context.Background()

	// Ensure _global project exists
	if err := store.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	// Seed some global memories with near-duplicates (all similar to the first one)
	// Using Jaccard >= 0.5 similarity
	_, _, _, err = store.UpsertWithOptions(ctx, "_global", "fact", "mr-slave alerting is alert-only via Telegram", "reflection", 0.9, []string{}, UpsertOptions{})
	if err != nil {
		t.Fatalf("seed 1: %v", err)
	}
	_, _, _, err = store.UpsertWithOptions(ctx, "_global", "fact", "mr-slave alerting is alert-only never auto-repair", "reflection", 0.7, []string{}, UpsertOptions{})
	if err != nil {
		t.Fatalf("seed 2: %v", err)
	}
	_, _, _, err = store.UpsertWithOptions(ctx, "_global", "fact", "mr-slave alerting is alert-only with dingo core repair", "reflection", 0.7, []string{}, UpsertOptions{})
	if err != nil {
		t.Fatalf("seed 3: %v", err)
	}
	// A distinct memory that should not be folded
	_, _, _, err = store.UpsertWithOptions(ctx, "_global", "fact", "FTS5 uses porter stemmer tokenizer", "reflection", 0.8, []string{}, UpsertOptions{})
	if err != nil {
		t.Fatalf("seed 4: %v", err)
	}

	// Dry run
	result, err := store.ConsolidateGlobal(ctx, GlobalConsolidationOptions{DryRun: true})
	if err != nil {
		t.Fatalf("ConsolidateGlobal dry-run: %v", err)
	}

	if !result.DryRun {
		t.Error("DryRun should be true")
	}
	if len(result.Clusters) != 1 {
		t.Errorf("expected 1 cluster, got %d", len(result.Clusters))
	}
	if len(result.Clusters[0].Folded) != 2 {
		t.Errorf("expected 2 folded memories, got %d", len(result.Clusters[0].Folded))
	}
	if result.Folded != 0 {
		t.Error("Folded should be 0 in dry-run")
	}

	// Verify the original memories still exist
	count, err := store.CountMemories(ctx, "_global")
	if err != nil {
		t.Fatalf("CountMemories: %v", err)
	}
	if count != 4 {
		t.Errorf("expected 4 memories after dry-run, got %d", count)
	}
}

// TestConsolidateGlobal_Apply tests that apply writes the folds.
func TestConsolidateGlobal_Apply(t *testing.T) {
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	store := NewStore(db, nil)
	ctx := context.Background()

	// Ensure _global project exists
	if err := store.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	// Seed some global memories with near-duplicates (all similar to the first one)
	_, _, _, err = store.UpsertWithOptions(ctx, "_global", "fact", "mr-slave alerting is alert-only via Telegram", "reflection", 0.9, []string{}, UpsertOptions{})
	if err != nil {
		t.Fatalf("seed 1: %v", err)
	}
	_, _, _, err = store.UpsertWithOptions(ctx, "_global", "fact", "mr-slave alerting is alert-only never auto-repair", "reflection", 0.7, []string{}, UpsertOptions{})
	if err != nil {
		t.Fatalf("seed 2: %v", err)
	}
	_, _, _, err = store.UpsertWithOptions(ctx, "_global", "fact", "mr-slave alerting is alert-only with dingo core repair", "reflection", 0.7, []string{}, UpsertOptions{})
	if err != nil {
		t.Fatalf("seed 3: %v", err)
	}
	// A distinct memory that should not be folded
	_, _, _, err = store.UpsertWithOptions(ctx, "_global", "fact", "FTS5 uses porter stemmer tokenizer", "reflection", 0.8, []string{}, UpsertOptions{})
	if err != nil {
		t.Fatalf("seed 4: %v", err)
	}

	// Apply
	result, err := store.ConsolidateGlobal(ctx, GlobalConsolidationOptions{DryRun: false})
	if err != nil {
		t.Fatalf("ConsolidateGlobal apply: %v", err)
	}

	if result.DryRun {
		t.Error("DryRun should be false")
	}
	if len(result.Clusters) != 1 {
		t.Errorf("expected 1 cluster, got %d", len(result.Clusters))
	}
	if len(result.Clusters[0].Folded) != 2 {
		t.Errorf("expected 2 folded memories, got %d", len(result.Clusters[0].Folded))
	}
	if result.Folded != 2 {
		t.Errorf("expected Folded=2, got %d", result.Folded)
	}

	// Verify only 2 memories remain (1 survivor + 1 distinct)
	count, err := store.CountMemories(ctx, "_global")
	if err != nil {
		t.Fatalf("CountMemories: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 memories after apply, got %d", count)
	}

	// Verify survivor has max importance and union of tags
	rows, err := store.GetAll(ctx, "_global", -1)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	for _, m := range rows {
		if m.Content == "FTS5 uses porter stemmer tokenizer" {
			continue // the distinct one
		}
		// This should be the survivor
		if m.Importance != 0.9 {
			t.Errorf("survivor importance should be 0.9 (max), got %.1f", m.Importance)
		}
	}
}

// TestConsolidateGlobal_PinnedSurvives tests that a pinned row always survives its cluster.
func TestConsolidateGlobal_PinnedSurvives(t *testing.T) {
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	store := NewStore(db, nil)
	ctx := context.Background()

	// Ensure _global project exists
	if err := store.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	// Seed: pinned memory (older) and non-pinned near-duplicate (newer)
	id1, _, _, err := store.UpsertWithOptions(ctx, "_global", "fact", "pinned memory about backups", "reflection", 0.5, []string{}, UpsertOptions{})
	if err != nil {
		t.Fatalf("seed 1: %v", err)
	}
	// Pin the first one
	err = store.TogglePin(ctx, id1, true)
	if err != nil {
		t.Fatalf("TogglePin: %v", err)
	}

	_, _, _, err = store.UpsertWithOptions(ctx, "_global", "fact", "pinned memory about backups with more detail", "reflection", 0.9, []string{}, UpsertOptions{})
	if err != nil {
		t.Fatalf("seed 2: %v", err)
	}

	result, err := store.ConsolidateGlobal(ctx, GlobalConsolidationOptions{DryRun: false})
	if err != nil {
		t.Fatalf("ConsolidateGlobal: %v", err)
	}

	if len(result.Clusters) != 1 {
		t.Errorf("expected 1 cluster, got %d", len(result.Clusters))
	}
	// The pinned one should be the survivor
	survivor := result.Clusters[0].Survivor
	if !survivor.Pinned {
		t.Error("pinned memory should be the survivor")
	}
	if survivor.ID != id1 {
		t.Errorf("survivor ID should be the pinned one (%s), got %s", id1, survivor.ID)
	}
}

// TestConsolidateGlobal_DistinctUntouched tests that distinct rows are untouched.
func TestConsolidateGlobal_DistinctUntouched(t *testing.T) {
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	store := NewStore(db, nil)
	ctx := context.Background()

	// Ensure _global project exists
	if err := store.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	// Seed distinct memories (low similarity)
	_, _, _, err = store.UpsertWithOptions(ctx, "_global", "fact", "FTS5 uses porter stemmer tokenizer", "reflection", 0.8, []string{}, UpsertOptions{})
	if err != nil {
		t.Fatalf("seed 1: %v", err)
	}
	_, _, _, err = store.UpsertWithOptions(ctx, "_global", "fact", "SQLite WAL mode enables concurrent reads", "reflection", 0.7, []string{}, UpsertOptions{})
	if err != nil {
		t.Fatalf("seed 2: %v", err)
	}
	_, _, _, err = store.UpsertWithOptions(ctx, "_global", "preference", "use tabs not spaces", "reflection", 0.9, []string{}, UpsertOptions{})
	if err != nil {
		t.Fatalf("seed 3: %v", err)
	}

	result, err := store.ConsolidateGlobal(ctx, GlobalConsolidationOptions{DryRun: false})
	if err != nil {
		t.Fatalf("ConsolidateGlobal: %v", err)
	}

	if len(result.Clusters) != 0 {
		t.Errorf("expected 0 clusters for distinct memories, got %d", len(result.Clusters))
	}
	if result.Folded != 0 {
		t.Errorf("expected Folded=0, got %d", result.Folded)
	}

	count, err := store.CountMemories(ctx, "_global")
	if err != nil {
		t.Fatalf("CountMemories: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3 memories unchanged, got %d", count)
	}
}

// TestConsolidateGlobal_HistoryAndProvenance tests that history and provenance are recorded.
func TestConsolidateGlobal_HistoryAndProvenance(t *testing.T) {
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	store := NewStore(db, nil)
	ctx := context.Background()

	// Ensure _global project exists
	if err := store.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	// Seed with specific agent/session (make them similar enough to fold)
	_, _, _, err = store.UpsertWithOptions(ctx, "_global", "fact", "memory one about backups via Telegram", "reflection", 0.7, []string{}, UpsertOptions{
		Provenance: Provenance{Agent: "opencode", SessionID: "session-1", SourceRef: "ref-1"},
	})
	if err != nil {
		t.Fatalf("seed 1: %v", err)
	}
	id2, _, _, err := store.UpsertWithOptions(ctx, "_global", "fact", "memory one about backups never auto-repair", "reflection", 0.8, []string{}, UpsertOptions{
		Provenance: Provenance{Agent: "opencode", SessionID: "session-2", SourceRef: "ref-2"},
	})
	if err != nil {
		t.Fatalf("seed 2: %v", err)
	}

	_, err = store.ConsolidateGlobal(ctx, GlobalConsolidationOptions{DryRun: false})
	if err != nil {
		t.Fatalf("ConsolidateGlobal: %v", err)
	}

	// Check history on survivor
	history, err := store.MemoryHistory(ctx, id2, 10)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	foundMerge := false
	for _, h := range history {
		if h.Phase == "merge" {
			foundMerge = true
			if h.MergedContent != "memory one about backups via Telegram" {
				t.Errorf("merged_content should be the folded text, got %q", h.MergedContent)
			}
			if h.Agent != "opencode" || h.SessionID != "session-1" {
				t.Errorf("provenance should be from folded memory, got agent=%s session=%s", h.Agent, h.SessionID)
			}
		}
	}
	if !foundMerge {
		t.Error("expected phaseMerge history entry on survivor")
	}

	// Check evidence on survivor
	prov, err := store.MemoryProvenance(ctx, id2)
	if err != nil {
		t.Fatalf("MemoryProvenance: %v", err)
	}
	foundObserved := false
	for _, p := range prov {
		if p.Kind == "observed" && p.Agent == "opencode" && p.SessionID == "session-1" {
			foundObserved = true
		}
	}
	if !foundObserved {
		t.Error("expected 'observed' evidence from folded memory on survivor")
	}
}

// TestConsolidateGlobal_SurvivorSelection tests the survivor selection priority:
// pinned > newest created_at > highest importance > longest content.
func TestConsolidateGlobal_SurvivorSelection(t *testing.T) {
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	store := NewStore(db, nil)
	ctx := context.Background()

	// Ensure _global project exists
	if err := store.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	// Test: newest created_at wins when none pinned
	_, _, _, err = store.UpsertWithOptions(ctx, "_global", "fact", "memory about deploy with more words", "reflection", 0.7, []string{}, UpsertOptions{})
	if err != nil {
		t.Fatalf("seed 1: %v", err)
	}
	// Small delay to ensure different created_at
	time.Sleep(10 * time.Millisecond)
	idNewer, _, _, err := store.UpsertWithOptions(ctx, "_global", "fact", "memory about deploy with more words and detail", "reflection", 0.6, []string{}, UpsertOptions{})
	if err != nil {
		t.Fatalf("seed 2: %v", err)
	}

	result, err := store.ConsolidateGlobal(ctx, GlobalConsolidationOptions{DryRun: false})
	if err != nil {
		t.Fatalf("ConsolidateGlobal: %v", err)
	}

	if len(result.Clusters) != 1 {
		t.Errorf("expected 1 cluster, got %d", len(result.Clusters))
	}
	// Newer should win
	if result.Clusters[0].Survivor.ID != idNewer {
		t.Errorf("survivor should be newer memory (%s), got %s", idNewer, result.Clusters[0].Survivor.ID)
	}
}

// TestConsolidateGlobal_Empty tests empty global scope.
func TestConsolidateGlobal_Empty(t *testing.T) {
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	store := NewStore(db, nil)
	ctx := context.Background()

	// Ensure _global project exists
	if err := store.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	result, err := store.ConsolidateGlobal(ctx, GlobalConsolidationOptions{DryRun: true})
	if err != nil {
		t.Fatalf("ConsolidateGlobal: %v", err)
	}

	if len(result.Clusters) != 0 {
		t.Errorf("expected 0 clusters for empty global, got %d", len(result.Clusters))
	}
}

// TestConsolidateGlobal_NumericConflict tests that numeric conflicts prevent folding.
func TestConsolidateGlobal_NumericConflict(t *testing.T) {
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	store := NewStore(db, nil)
	ctx := context.Background()

	// Ensure _global project exists
	if err := store.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	// Two memories differing only in a numeric value (port)
	_, _, _, err = store.UpsertWithOptions(ctx, "_global", "fact", "service listens on port 8080", "reflection", 0.7, []string{}, UpsertOptions{})
	if err != nil {
		t.Fatalf("seed 1: %v", err)
	}
	_, _, _, err = store.UpsertWithOptions(ctx, "_global", "fact", "service listens on port 8081", "reflection", 0.8, []string{}, UpsertOptions{})
	if err != nil {
		t.Fatalf("seed 2: %v", err)
	}

	result, err := store.ConsolidateGlobal(ctx, GlobalConsolidationOptions{DryRun: false})
	if err != nil {
		t.Fatalf("ConsolidateGlobal: %v", err)
	}

	// Should NOT be folded due to numeric conflict
	if len(result.Clusters) != 0 {
		t.Errorf("expected 0 clusters due to numeric conflict, got %d", len(result.Clusters))
	}

	count, err := store.CountMemories(ctx, "_global")
	if err != nil {
		t.Fatalf("CountMemories: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 memories (no fold due to numeric conflict), got %d", count)
	}
}