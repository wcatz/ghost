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

func TestProjectTasksResource_ListTasksReadError(t *testing.T) {
	db, store, _ := newStoreWithDB(t)
	plantTask(t, db, "abc123", "seeded-open-task", "seeded open task", "a task that should have been seen", "active")
	wrapped := listTasksFailingStore{MemoryStore: store, err: errors.New("boom: tasks read failed")}
	srv := New(wrapped, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), "test")
	session := connectedClient(t, srv)
	_, err := session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "ghost://project/abc123/tasks"})
	if err == nil || !strings.Contains(err.Error(), "boom: tasks read failed") || !strings.Contains(err.Error(), "list tasks") {
		t.Fatalf("unexpected error: %v", err)
	}
}
