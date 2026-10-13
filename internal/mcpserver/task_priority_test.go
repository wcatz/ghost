package mcpserver

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
)

// A task's priority is a 0-4 value with a CHECK constraint in the schema, and
// 2 is the documented default. An out-of-range value on create or update must
// be refused by name rather than silently reset to 2, which demotes a task the
// caller marked critical on a typo.
func TestGhostTaskCreate_RejectsOutOfRangePriority(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)
	ctx := context.Background()

	for _, priority := range []int{-1, 5, 9} {
		result := callTool(t, session, "ghost_task_create", map[string]any{
			"project_id": "abc123",
			"title":      "ship the release",
			"priority":   priority,
		})
		if !result.IsError {
			t.Fatalf("create with priority %d was accepted: %s", priority, resultText(result))
		}
		msg := resultText(result)
		if !strings.Contains(msg, "priority") || !strings.Contains(msg, "0 and 4") {
			t.Errorf("create refusal for priority %d must name the 0-4 range, got: %s", priority, msg)
		}
	}

	tasks, err := store.ListTasks(ctx, "abc123", "", 10)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("a refused create must store nothing, found %d row(s)", len(tasks))
	}

	// The boundary values remain valid, so the refusal is a range check and
	// not a rejection of priority itself.
	for _, priority := range []int{0, 4} {
		result := callTool(t, session, "ghost_task_create", map[string]any{
			"project_id": "abc123",
			"title":      "ship the release",
			"priority":   priority,
		})
		if result.IsError {
			t.Fatalf("create with in-range priority %d was rejected: %s", priority, resultText(result))
		}
	}
	tasks, err = store.ListTasks(ctx, "abc123", "", 10)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("expected the two boundary creates to persist, found %d row(s)", len(tasks))
	}
}

func TestGhostTaskUpdate_RejectsOutOfRangePriority(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)
	ctx := context.Background()

	id, err := store.CreateTask(ctx, "abc123", "Fix the bug", "critical", 0)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	for _, priority := range []int{-1, 5, 9} {
		result := callTool(t, session, "ghost_task_update", map[string]any{
			"task_id":  id,
			"priority": priority,
		})
		if !result.IsError {
			t.Fatalf("update with priority %d was accepted: %s", priority, resultText(result))
		}
		msg := resultText(result)
		if !strings.Contains(msg, "priority") || !strings.Contains(msg, "0 and 4") {
			t.Errorf("update refusal for priority %d must name the 0-4 range, got: %s", priority, msg)
		}
	}

	task, err := store.GetTask(ctx, id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Priority != 0 {
		t.Errorf("a refused update must leave the stored priority unchanged, got %d, want 0", task.Priority)
	}

	// A valid priority still applies, so the change is a range check and not a
	// blanket refusal of the field.
	result := callTool(t, session, "ghost_task_update", map[string]any{
		"task_id":  id,
		"priority": 4,
	})
	if result.IsError {
		t.Fatalf("update with in-range priority 4 was rejected: %s", resultText(result))
	}
	task, err = store.GetTask(ctx, id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Priority != 4 {
		t.Errorf("priority after valid update = %d, want 4", task.Priority)
	}
}
