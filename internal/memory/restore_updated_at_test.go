package memory

import (
	"context"
	"testing"
)

func TestRestoreSnapshotPutsUpdatedAtBack(t *testing.T) {
	t.Run("reinserted", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		id, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: "first mcp", Source: "mcp", Importance: 0.5, Tags: []string{}})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE memories SET created_at='2026-01-01 00:00:00', updated_at='2026-06-15 12:00:00' WHERE id=?`, id); err != nil {
			t.Fatalf("stamp: %v", err)
		}
		if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{Category: "fact", Content: "consolidated replacement", Importance: 0.6, Tags: []string{}}}, "", nil); err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}
		if rowExists(t, s, id) {
			t.Fatal("precondition: row not consumed")
		}
		if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
			t.Fatalf("RestoreSnapshot: %v", err)
		}
		if after := getOne(t, s, id); after.UpdatedAt != "2026-06-15 12:00:00" {
			t.Errorf("updated_at = %q, want %q", after.UpdatedAt, "2026-06-15 12:00:00")
		}
	})
	t.Run("in-place reused", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		id, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: "second mcp", Source: "mcp", Importance: 0.5, Tags: []string{}})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE memories SET created_at='2026-02-01 00:00:00', updated_at='2026-07-15 12:00:00' WHERE id=?`, id); err != nil {
			t.Fatalf("stamp: %v", err)
		}
		if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{Category: "fact", Content: "second mcp", Importance: 0.9, Tags: []string{}}}, "", nil); err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}
		if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
			t.Fatalf("RestoreSnapshot: %v", err)
		}
		if after := getOne(t, s, id); after.UpdatedAt != "2026-07-15 12:00:00" {
			t.Errorf("updated_at = %q, want %q", after.UpdatedAt, "2026-07-15 12:00:00")
		}
	})
}

func TestRestoreSnapshotLeavesUpdatedAtAloneOnLegacySnapshot(t *testing.T) {
	t.Run("reinserted", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		id, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: "a fact whose snapshot predates v29", Source: "reflection", Importance: 0.5})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE memories SET created_at='2026-01-01 00:00:00', updated_at='2026-06-15 12:00:00' WHERE id=?`, id); err != nil {
			t.Fatalf("stamp: %v", err)
		}
		if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{Category: "fact", Content: "consolidated replacement", Importance: 0.6, Tags: []string{}}}, "", nil); err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE memory_snapshots SET updated_at = NULL WHERE memory_id = ?`, id); err != nil {
			t.Fatalf("null stamp: %v", err)
		}
		if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
			t.Fatalf("RestoreSnapshot: %v", err)
		}
		if after := getOne(t, s, id); after.UpdatedAt != "2026-01-01 00:00:00" {
			t.Errorf("updated_at = %q, want %q", after.UpdatedAt, "2026-01-01 00:00:00")
		}
	})
	t.Run("in-place", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		id, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: "a fact kept in place", Source: "reflection", Importance: 0.5})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE memories SET created_at='2026-02-01 00:00:00', updated_at='2026-07-15 12:00:00' WHERE id=?`, id); err != nil {
			t.Fatalf("stamp: %v", err)
		}
		if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{Category: "fact", Content: "a fact kept in place", Importance: 0.9, Tags: []string{}}}, "", nil); err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}
		live := getOne(t, s, id).UpdatedAt
		if _, err := s.db.ExecContext(ctx, `UPDATE memory_snapshots SET updated_at = NULL WHERE memory_id = ?`, id); err != nil {
			t.Fatalf("null stamp: %v", err)
		}
		if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
			t.Fatalf("RestoreSnapshot: %v", err)
		}
		if after := getOne(t, s, id); after.UpdatedAt != live {
			t.Errorf("updated_at = %q, want %q", after.UpdatedAt, live)
		}
	})
}
