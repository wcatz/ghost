package mcpserver

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/provider"
)

// misownedRowStore reports a row under owner for any id the handler looks up,
// though none is stored, so the handler's ownership check is what refuses the
// call. With tombstone set the row reads as deleted and its recorded history
// names owner instead: the already-deleted branch of a purge.
type misownedRowStore struct {
	*memory.Store
	owner     string
	tombstone bool
}

func (s *misownedRowStore) GetByIDs(_ context.Context, ids []string) ([]memory.Memory, error) {
	if s.tombstone {
		return nil, nil
	}
	out := make([]memory.Memory, 0, len(ids))
	for _, id := range ids {
		out = append(out, memory.Memory{ID: id, ProjectID: s.owner, Category: "fact", Content: "elsewhere"})
	}
	return out, nil
}

func (s *misownedRowStore) MemoryHistory(_ context.Context, memoryID string, _ int) ([]memory.HistoryEntry, error) {
	return []memory.HistoryEntry{{MemoryID: memoryID, ProjectID: s.owner, Phase: "delete", Content: "elsewhere", Category: "fact"}}, nil
}

// TestOwnershipRefusalsNeverEchoTheID: a memory_id that is token-shaped or
// longer than the refusal bound is not repeated when ghost_memory_update,
// ghost_memory_promote, ghost_memory_delete or ghost_memory_pin refuses it —
// whether the handler finds the row in another project (or already global, or
// deleted with history filed under another project), or the store's write finds
// it gone from the project. The call is still an error and still says what was
// refused.
func TestOwnershipRefusalsNeverEchoTheID(t *testing.T) {
	ids := map[string]string{
		"token-shaped": echoToken,
		"oversized":    strings.Repeat("z", memory.ArgEchoMaxBytes+44),
	}
	type call struct {
		tool string
		args map[string]any
		want string
	}
	update := map[string]any{"importance": 0.7}
	cases := []struct {
		name  string
		store func(base *memory.Store, project string) provider.MemoryStore
		calls []call
	}{
		{
			name: "row in another project",
			store: func(base *memory.Store, _ string) provider.MemoryStore {
				return &misownedRowStore{Store: base, owner: "someone-else"}
			},
			calls: []call{
				{"ghost_memory_update", update, "does not belong to project"},
				{"ghost_memory_promote", nil, "does not belong to project"},
				{"ghost_memory_delete", nil, "does not belong to project"},
				{"ghost_memory_pin", map[string]any{"pinned": true}, "does not belong to project"},
			},
		},
		{
			name: "row already global",
			store: func(base *memory.Store, _ string) provider.MemoryStore {
				return &misownedRowStore{Store: base, owner: memory.GlobalProjectID}
			},
			calls: []call{
				{"ghost_memory_promote", nil, "already global"},
			},
		},
		{
			name: "deleted row filed under another project",
			store: func(base *memory.Store, _ string) provider.MemoryStore {
				return &misownedRowStore{Store: base, owner: "someone-else", tombstone: true}
			},
			calls: []call{
				{"ghost_memory_delete", map[string]any{"purge_history": true}, "does not belong to project"},
			},
		},
		{
			name: "row gone before the store's write",
			store: func(base *memory.Store, project string) provider.MemoryStore {
				return &vanishedRowStore{Store: base, projectID: project}
			},
			calls: []call{
				{"ghost_memory_update", update, "not found in project"},
				{"ghost_memory_promote", nil, "not found in project"},
			},
		},
	}
	for _, tc := range cases {
		base := testStore(t)
		resolved, _, err := base.ResolveProject(context.Background(), "test-project")
		if err != nil || resolved == "" {
			t.Fatalf("ResolveProject: %q %v", resolved, err)
		}
		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
		srv := New(tc.store(base, resolved), logger, "test")
		session := connectedClient(t, srv)
		for shape, id := range ids {
			for _, c := range tc.calls {
				args := map[string]any{"project_id": "test-project", "memory_id": id}
				for k, v := range c.args {
					args[k] = v
				}
				out := callToolErr(t, session, c.tool, args)
				if !strings.Contains(out, c.want) {
					t.Errorf("%s, %s, %s: refusal does not say %q: %.200q", tc.name, c.tool, shape, c.want, out)
				}
				if strings.Contains(out, id) {
					t.Errorf("%s, %s, %s: refusal repeats the memory_id: %.200q", tc.name, c.tool, shape, out)
				}
			}
		}
	}
}
