package mcpserver

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

type vanishedRowStore struct {
	*memory.Store
	projectID string
}

func (s *vanishedRowStore) GetByIDs(_ context.Context, ids []string) ([]memory.Memory, error) {
	out := make([]memory.Memory, 0, len(ids))
	for _, id := range ids {
		out = append(out, memory.Memory{ID: id, ProjectID: s.projectID, Category: "fact", Content: "gone"})
	}
	return out, nil
}

func TestMemoryDeleteAndPinNotFoundNeverEchoTheID(t *testing.T) {
	ids := map[string]string{
		"token-shaped": echoToken,
		"oversized":    strings.Repeat("z", memory.ArgEchoMaxBytes+44),
	}
	calls := []struct {
		name string
		tool string
		args map[string]any
	}{
		{"delete", "ghost_memory_delete", map[string]any{}},
		{"delete purge_history", "ghost_memory_delete", map[string]any{"purge_history": true}},
		{"pin", "ghost_memory_pin", map[string]any{"pinned": true}},
	}
	for _, vanished := range []bool{false, true} {
		store := testStore(t)
		resolved, _, err := store.ResolveProject(context.Background(), "test-project")
		if err != nil || resolved == "" {
			t.Fatalf("ResolveProject: %q %v", resolved, err)
		}
		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
		srv := New(store, logger, "test")
		where := "handler read"
		if vanished {
			srv = New(&vanishedRowStore{Store: store, projectID: resolved}, logger, "test")
			where = "store write"
		}
		session := connectedClient(t, srv)
		for shape, id := range ids {
			for _, c := range calls {
				args := map[string]any{"project_id": "test-project", "memory_id": id}
				for k, v := range c.args {
					args[k] = v
				}
				out := callToolErr(t, session, c.tool, args)
				if !strings.Contains(out, "not found") {
					t.Errorf("%s, %s, %s: refusal does not say not found: %.200q", where, c.name, shape, out)
				}
				if strings.Contains(out, id) {
					t.Errorf("%s, %s, %s: refusal repeats the memory_id: %.200q", where, c.name, shape, out)
				}
			}
		}
	}
}
