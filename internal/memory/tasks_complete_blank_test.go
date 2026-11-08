package memory

import (
	"context"
	"testing"
)

func TestCompleteTaskWhitespaceOnlyNotesLeaveStoredNotesUntouched(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, err := s.CreateTask(ctx, testProject, "t", "d", 2)
	if err != nil {
		t.Fatal(err)
	}
	const keep = "keep notes"
	if _, err := s.CompleteTask(ctx, id, keep); err != nil {
		t.Fatal(err)
	}
	for _, notes := range []string{"\t", "\n", "\r\n", "\u00a0", "\u2003", " \t\n\r\u00a0\u2003 "} {
		if _, err := s.CompleteTask(ctx, id, notes); err != nil {
			t.Fatalf("complete with %q: %v", notes, err)
		}
		got, err := s.GetTask(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Notes != keep {
			t.Fatalf("notes after %q = %q, want %q", notes, got.Notes, keep)
		}
		if got.Status != "done" {
			t.Fatalf("status = %q", got.Status)
		}
	}
}
