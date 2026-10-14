package mcpserver

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/provider"
)

// listTasksFailingStore wraps a real store and fails ListTasks for one status,
// so the project-tasks resource's handling of a read failure can be asserted
// without touching SQLite.
type listTasksFailingStore struct {
	provider.MemoryStore
	err error
}

func (f listTasksFailingStore) ListTasks(ctx context.Context, projectID, status string, limit int) ([]memory.Task, error) {
	if status == "active" {
		return nil, f.err
	}
	return f.MemoryStore.ListTasks(ctx, projectID, status, limit)
}

// TestProjectTasksResource_ListTasksReadError proves the project-tasks resource
// reports a failed ListTasks read as a failure rather than answering
// "No open tasks for this project." An active task is seeded so that, before
// the fix, the failed read was swallowed and the answer was empty.
func TestProjectTasksResource_ListTasksReadError(t *testing.T) {
	db, store, _ := newStoreWithDB(t)
	plantTask(t, db, "abc123", "seeded-open-task", "seeded open task", "a task that should have been seen", "active")

	boom := errors.New("boom: tasks read failed")
	wrapped := listTasksFailingStore{MemoryStore: store, err: boom}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(wrapped, logger, "test")
	session := connectedClient(t, srv)

	rr, err := session.ReadResource(context.Background(), &mcp.ReadResourceParams{
		URI: "ghost://project/abc123/tasks",
	})
	if err == nil {
		t.Fatalf("ReadResource tasks: got nil error, want the ListTasks failure")
	}
	if !strings.Contains(err.Error(), "boom: tasks read failed") {
		t.Errorf("error does not name the underlying failure: %v", err)
	}
	if !strings.Contains(err.Error(), "list tasks") {
		t.Errorf("error does not name the failing read: %v", err)
	}
	if rr != nil {
		for _, c := range rr.Contents {
			if strings.Contains(c.Text, "No open tasks") {
				t.Errorf("a failed read was reported as 'No open tasks': %s", c.Text)
			}
		}
	}
}
