package mcpserver

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
)

func TestGhostTaskPriorityValidation(t *testing.T) {
	store := testStore(t)
	srv := New(store, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), "test")
	session := connectedClient(t, srv)
	ctx := context.Background()

	for _, priority := range []int{-1, 5} {
		res := callTool(t, session, "ghost_task_create", map[string]any{"project_id": "abc123", "title": "ship the release", "priority": priority})
		if !res.IsError || !strings.Contains(resultText(res), "0 and 4") {
			t.Fatalf("create %d: %q", priority, resultText(res))
		}
	}
	if tasks, _ := store.ListTasks(ctx, "abc123", "", 10); len(tasks) != 0 {
		t.Fatalf("refused create stored %d rows", len(tasks))
	}
	for _, priority := range []int{0, 4} {
		if res := callTool(t, session, "ghost_task_create", map[string]any{"project_id": "abc123", "title": "ship", "priority": priority}); res.IsError {
			t.Fatalf("create %d: %q", priority, resultText(res))
		}
	}

	id, err := store.CreateTask(ctx, "abc123", "Fix the bug", "critical", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, priority := range []int{-1, 5} {
		res := callTool(t, session, "ghost_task_update", map[string]any{"task_id": id, "priority": priority})
		if !res.IsError || !strings.Contains(resultText(res), "0 and 4") {
			t.Fatalf("update %d: %q", priority, resultText(res))
		}
	}
	task, _ := store.GetTask(ctx, id)
	if task.Priority != 0 {
		t.Fatalf("priority changed to %d", task.Priority)
	}
	if res := callTool(t, session, "ghost_task_update", map[string]any{"task_id": id, "priority": 4}); res.IsError {
		t.Fatalf("valid update: %q", resultText(res))
	}
}
