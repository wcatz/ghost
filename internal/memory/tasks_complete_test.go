package memory

import (
	"context"
	"testing"
)

// TestCompleteTaskIsIdempotentOnADoneTask pins #1143: completing a task that is
// already done must not erase its notes or move its completed_at, and a
// non-empty notes value replaces the notes without moving the stamp.
func TestCompleteTaskIsIdempotentOnADoneTask(t *testing.T) {
	ctx := context.Background()
	const backdated = "2026-01-01 00:00:00"

	newDone := func(t *testing.T, s *Store, notes string) string {
		t.Helper()
		id, err := s.CreateTask(ctx, testProject, "fix the flake", "", 2)
		if err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		if _, err := s.CompleteTask(ctx, id, notes); err != nil {
			t.Fatalf("CompleteTask: %v", err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET completed_at = ? WHERE id = ?`, backdated, id); err != nil {
			t.Fatalf("backdate: %v", err)
		}
		return id
	}

	t.Run("pending task is stamped now", func(t *testing.T) {
		s := testStore(t)
		id, err := s.CreateTask(ctx, testProject, "t", "", 2)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.CompleteTask(ctx, id, "why"); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetTask(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "done" || got.Notes != "why" || got.CompletedAt == "" {
			t.Errorf("pending completion: %+v", got)
		}
	})

	t.Run("repeat with empty notes keeps notes and stamp", func(t *testing.T) {
		s := testStore(t)
		id := newDone(t, s, "root cause: closed conn")
		if _, err := s.CompleteTask(ctx, id, ""); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetTask(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Notes != "root cause: closed conn" {
			t.Errorf("notes = %q, want the original kept", got.Notes)
		}
		if got.CompletedAt != backdated {
			t.Errorf("completed_at = %q, want %q kept", got.CompletedAt, backdated)
		}
	})

	t.Run("repeat with new notes replaces notes and keeps stamp", func(t *testing.T) {
		s := testStore(t)
		id := newDone(t, s, "first")
		if _, err := s.CompleteTask(ctx, id, "second"); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetTask(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Notes != "second" {
			t.Errorf("notes = %q, want replaced", got.Notes)
		}
		if got.CompletedAt != backdated {
			t.Errorf("completed_at = %q, want %q kept", got.CompletedAt, backdated)
		}
	})
}
