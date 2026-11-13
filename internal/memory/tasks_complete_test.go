package memory

import (
	"context"
	"testing"
)

// TestCompleteTaskIsIdempotentOnADoneTask pins #1143: completing a task that is
// already done must not erase its notes or move its completed_at, and a
// non-empty notes value replaces the notes without moving the stamp. Notes that
// are only whitespace count as empty.
func TestCompleteTaskIsIdempotentOnADoneTask(t *testing.T) {
	ctx := context.Background()
	const backdated = "2026-01-01 00:00:00"

	for _, tc := range []struct{ name, repeat, wantNotes string }{
		{"empty notes keep notes", "", "first"},
		{"whitespace notes keep notes", " \t\n\r   ", "first"},
		{"new notes replace notes", "second", "second"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			id, err := s.CreateTask(ctx, testProject, "fix the flake", "", 2)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.CompleteTask(ctx, id, "first"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET completed_at = ? WHERE id = ?`, backdated, id); err != nil {
				t.Fatal(err)
			}
			if _, err := s.CompleteTask(ctx, id, tc.repeat); err != nil {
				t.Fatal(err)
			}
			got, err := s.GetTask(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != "done" || got.Notes != tc.wantNotes || got.CompletedAt != backdated {
				t.Errorf("task = %+v, want notes %q and completed_at %q kept", got, tc.wantNotes, backdated)
			}
		})
	}
}
