package memory

import (
	"context"
	"testing"
)

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
		t.Fatalf("ListTasks limit 1 = %+v, %v", tasks, err)
	}
	decs, err := s.ListDecisions(ctx, testProject, "", 1)
	if err != nil || len(decs) != 1 || decs[0].Title != "decision third" {
		t.Fatalf("ListDecisions limit 1 = %+v, %v", decs, err)
	}
	decs, err = s.ListDecisions(ctx, testProject, "", 10)
	if err != nil || len(decs) != 3 || decs[0].Title != "decision third" || decs[2].Title != "decision first" {
		t.Fatalf("ListDecisions order = %+v, %v", decs, err)
	}
	live, err := s.LiveDecisions(ctx, testProject, 1)
	if err != nil || len(live) != 1 || live[0].Title != "decision third" {
		t.Fatalf("LiveDecisions limit 1 = %+v, %v", live, err)
	}
}
