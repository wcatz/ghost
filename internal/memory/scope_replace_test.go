package memory

import (
	"context"
	"testing"
)

// TestReplaceNonManualCarriesScope is issue #572.
//
// Schema v12 added memories.scope, but the reflection replace's INSERT never
// named the column, so a consolidated memory came out of a replace with its
// scope silently NULL. A scope is the difference between "true in production"
// and "true anywhere", and reflection is exactly the pass that decides which
// knowledge generalises — so the one writer that most needs to record scope
// was the one dropping it.
func TestReplaceNonManualCarriesScope(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	scope := map[string]string{"environment": "production"}

	// A memory the replace will delete outright (nothing to reuse it onto).
	if _, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "an old fact with no scope at all",
		Source: "reflection", Importance: 0.5,
	}); err != nil {
		t.Fatalf("create old: %v", err)
	}

	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "fact", Content: "a consolidated fact scoped to production", Importance: 0.6, Scope: scope},
	}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	all, err := s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("project holds %d memories, want 1", len(all))
	}
	if all[0].Scope["environment"] != "production" {
		t.Errorf("scope = %v, want environment=production — the replace INSERT never named the scope column", all[0].Scope)
	}
}

// TestReplaceNonManualRewritesScopeOnReusedRow covers the other branch. The
// reuse path updates a row in place, and its UPDATE does not mention scope —
// so the row keeps whatever scope it had before, which is only correct by
// accident, when the old and new scope happen to agree. When reflection
// narrows or widens a scope, the replace must record what it decided.
func TestReplaceNonManualRewritesScopeOnReusedRow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	old, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "a fact that reflection will rewrite in place",
		Source: "reflection", Importance: 0.5, Scope: map[string]string{"environment": "staging"},
	})
	if err != nil {
		t.Fatalf("create old: %v", err)
	}

	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "fact", Content: "a fact that reflection will rewrite in place", Importance: 0.6,
			Scope: map[string]string{"environment": "production"}},
	}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	rows, err := s.GetByIDs(ctx, []string{old})
	if err != nil || len(rows) == 0 {
		t.Fatalf("GetByIDs: %v", err)
	}
	if rows[0].Scope["environment"] != "production" {
		t.Errorf("scope = %v, want environment=production — the reuse UPDATE leaves the old scope behind", rows[0].Scope)
	}
}

// TestRestoreSnapshotBringsScopeBack is the half that made the first defect
// unrecoverable. memory_snapshots had no scope column at all, so the only undo
// history for a reflection replace could not carry the thing being restored.
// A restore that cannot bring scope back turns a lost scope into a permanent
// loss, which is why this is one issue and not two.
func TestRestoreSnapshotBringsScopeBack(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if _, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "a scoped fact that will be snapshotted and replaced away",
		Source: "reflection", Importance: 0.5, Scope: map[string]string{"environment": "production"},
	}); err != nil {
		t.Fatalf("create scoped: %v", err)
	}

	// ReplaceNonManual snapshots first, then destroys the row.
	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "fact", Content: "an unrelated consolidated fact", Importance: 0.6},
	}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	all, err := s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll after replace: %v", err)
	}
	for _, m := range all {
		if m.Content == "a scoped fact that will be snapshotted and replaced away" {
			t.Fatal("precondition: the scoped memory should have been replaced away")
		}
	}

	if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}

	all, err = s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll after restore: %v", err)
	}
	var restored *Memory
	for i := range all {
		if all[i].Content == "a scoped fact that will be snapshotted and replaced away" {
			restored = &all[i]
		}
	}
	if restored == nil {
		t.Fatalf("restore did not bring the fact back: %+v", all)
	}
	if restored.Scope["environment"] != "production" {
		t.Errorf("restored scope = %v, want environment=production — memory_snapshots cannot record scope", restored.Scope)
	}
}

// TestRestoreSnapshotRewritesScopeOnExistingRow: restore updates rows that
// still exist by matching snapshot to memory id. That UPDATE has to carry
// scope too, or a restore into a row whose scope was edited in the meantime
// leaves the edit in place and restores nothing about scope.
func TestRestoreSnapshotRewritesScopeOnExistingRow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "a fact whose scope will be edited after the snapshot",
		Source: "reflection", Importance: 0.5, Scope: map[string]string{"environment": "production"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "fact", Content: "a fact whose scope will be edited after the snapshot", Importance: 0.6,
			Scope: map[string]string{"environment": "staging"}},
	}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	rows, err := s.GetByIDs(ctx, []string{id})
	if err != nil || len(rows) == 0 {
		t.Fatalf("GetByIDs: %v", err)
	}
	if rows[0].Scope["environment"] != "staging" {
		t.Fatalf("precondition: scope = %v, want staging after the replace", rows[0].Scope)
	}

	if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}

	rows, err = s.GetByIDs(ctx, []string{id})
	if err != nil || len(rows) == 0 {
		t.Fatalf("GetByIDs after restore: %v", err)
	}
	if rows[0].Scope["environment"] != "production" {
		t.Errorf("restored scope = %v, want environment=production — the restore UPDATE does not carry scope",
			rows[0].Scope)
	}
}
