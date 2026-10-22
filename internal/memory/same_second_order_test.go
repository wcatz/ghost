package memory

import (
	"context"
	"testing"
)

// TestSameSecondTasksAndDecisionsListNewestFirst: created_at has one-second
// resolution, so rows written in a burst tie on it, and SQLite returns tied rows
// in scan (rowid, oldest-first) order. The lists promise newest first, so the
// last row of a burst must come back first, and a limit must drop the oldest.
// The stamps are forced equal so the tie does not depend on the wall clock.
func TestSameSecondTasksAndDecisionsListNewestFirst(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	for _, title := range []string{"t-old", "t-mid", "t-new"} {
		if _, err := s.CreateTask(ctx, testProject, title, "", 2); err != nil {
			t.Fatalf("CreateTask %q: %v", title, err)
		}
	}
	for _, title := range []string{"decision first", "decision second", "decision third"} {
		if _, _, _, err := s.RecordDecision(ctx, testProject, title, "body "+title, "why", nil, nil); err != nil {
			t.Fatalf("RecordDecision %q: %v", title, err)
		}
	}
	for _, table := range []string{"tasks", "decisions"} {
		if _, err := s.db.ExecContext(ctx, `UPDATE `+table+` SET created_at = '2026-01-02 03:04:05'`); err != nil {
			t.Fatalf("stamp %s: %v", table, err)
		}
	}

	tasks, err := s.ListTasks(ctx, testProject, "", 1)
	if err != nil || len(tasks) != 1 || tasks[0].Title != "t-new" {
		t.Errorf("ListTasks limit 1 = %+v, %v; want t-new", tasks, err)
	}
	decs, err := s.ListDecisions(ctx, testProject, "", 1)
	if err != nil || len(decs) != 1 || decs[0].Title != "decision third" {
		t.Errorf("ListDecisions limit 1 = %+v, %v; want decision third", decs, err)
	}
	decs, err = s.ListDecisions(ctx, testProject, "", 10)
	if err != nil || len(decs) != 3 || decs[0].Title != "decision third" || decs[2].Title != "decision first" {
		t.Errorf("ListDecisions order = %+v, %v; want third, second, first", decs, err)
	}
	live, err := s.LiveDecisions(ctx, testProject, 1)
	if err != nil || len(live) != 1 || live[0].Title != "decision third" {
		t.Errorf("LiveDecisions limit 1 = %+v, %v; want decision third", live, err)
	}
}
