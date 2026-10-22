package memory

import (
	"context"
	"testing"
)

// TestRestoreSnapshotPutsUpdatedAtBack: a snapshot this build wrote recorded a
// memory's updated_at, and both restore paths put it back. The reinserted row
// (deleted by the replace) comes back through the INSERT ... SELECT; the
// in-place row (reused by the replace) is rewritten through the UPDATE.
func TestRestoreSnapshotPutsUpdatedAtBack(t *testing.T) {
	t.Run("a reinserted row gets its original updated_at back", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()

		id, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: "first mcp", Source: "mcp", Importance: 0.5, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE memories SET created_at='2026-01-01 00:00:00', updated_at='2026-06-15 12:00:00' WHERE id=?`, id); err != nil {
			t.Fatalf("stamp: %v", err)
		}

		// An unrelated emission so the replace deletes the row above.
		if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
			{Category: "fact", Content: "consolidated replacement", Importance: 0.6, Tags: []string{}},
		}, "", nil); err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}
		if rowExists(t, s, id) {
			t.Fatalf("precondition: the replace did not consume the row")
		}

		if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
			t.Fatalf("RestoreSnapshot: %v", err)
		}
		if !rowExists(t, s, id) {
			t.Fatalf("restore did not bring the row back")
		}
		after := getOne(t, s, id)
		if after.UpdatedAt != "2026-06-15 12:00:00" {
			t.Errorf("updated_at = %q, want %q", after.UpdatedAt, "2026-06-15 12:00:00")
		}
	})

	t.Run("an in-place reused row gets its original updated_at back", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()

		id, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: "second mcp", Source: "mcp", Importance: 0.5, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE memories SET created_at='2026-02-01 00:00:00', updated_at='2026-07-15 12:00:00' WHERE id=?`, id); err != nil {
			t.Fatalf("stamp: %v", err)
		}

		// Re-emit the same content at a different importance: reusePreservesAge
		// keeps created_at but stamps updated_at, exercising the in-place path.
		if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
			{Category: "fact", Content: "second mcp", Importance: 0.9, Tags: []string{}},
		}, "", nil); err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}

		if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
			t.Fatalf("RestoreSnapshot: %v", err)
		}
		after := getOne(t, s, id)
		if after.UpdatedAt != "2026-07-15 12:00:00" {
			t.Errorf("updated_at = %q, want %q", after.UpdatedAt, "2026-07-15 12:00:00")
		}
	})
}

// TestRestoreSnapshotLeavesUpdatedAtAloneOnLegacySnapshot: a snapshot taken
// before v29 (NULL updated_at) restores a reinserted row with created_at and
// leaves an in-place row's live stamp untouched.
func TestRestoreSnapshotLeavesUpdatedAtAloneOnLegacySnapshot(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "a fact whose snapshot predates v29", Source: "reflection", Importance: 0.5,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE memories SET created_at='2026-01-01 00:00:00', updated_at='2026-06-15 12:00:00' WHERE id=?`, id); err != nil {
		t.Fatalf("stamp: %v", err)
	}

	// A replacement that drops the row, so it comes back through the INSERT.
	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "fact", Content: "consolidated replacement", Importance: 0.6, Tags: []string{}},
	}, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}
	if rowExists(t, s, id) {
		t.Fatalf("precondition: the replace did not consume the row")
	}

	// Mark the snapshot as pre-v29: NULL updated_at.
	if _, err := s.db.ExecContext(ctx, `UPDATE memory_snapshots SET updated_at = NULL WHERE memory_id = ?`, id); err != nil {
		t.Fatalf("null stamp: %v", err)
	}

	if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	if !rowExists(t, s, id) {
		t.Fatalf("restore did not bring the row back")
	}
	after := getOne(t, s, id)
	if after.UpdatedAt != "2026-01-01 00:00:00" {
		t.Errorf("reinserted updated_at = %q, want created_at %q", after.UpdatedAt, "2026-01-01 00:00:00")
	}

}

// TestRestoreSnapshotLegacyNullUpdatedAtInPlace: a snapshot taken before v29
// (NULL updated_at) leaves an in-place restored row's live stamp untouched.
func TestRestoreSnapshotLegacyNullUpdatedAtInPlace(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "a fact kept in place", Source: "reflection", Importance: 0.5,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE memories SET created_at='2026-02-01 00:00:00', updated_at='2026-07-15 12:00:00' WHERE id=?`, id); err != nil {
		t.Fatalf("stamp: %v", err)
	}

	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "fact", Content: "a fact kept in place", Importance: 0.9, Tags: []string{}},
	}, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}
	live := getOne(t, s, id).UpdatedAt

	// Mark the snapshot as pre-v29: NULL updated_at.
	if _, err := s.db.ExecContext(ctx, `UPDATE memory_snapshots SET updated_at = NULL WHERE memory_id = ?`, id); err != nil {
		t.Fatalf("null stamp: %v", err)
	}

	if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	after := getOne(t, s, id)
	if after.UpdatedAt != live {
		t.Errorf("updated_at = %q, want live value %q", after.UpdatedAt, live)
	}
}
