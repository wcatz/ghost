package mcpserver

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/provider"
)

type promoteAfterReadStore struct {
	*memory.Store
	armed  atomic.Bool
	target string
}

func (s *promoteAfterReadStore) GetByIDs(ctx context.Context, ids []string) ([]memory.Memory, error) {
	mems, err := s.Store.GetByIDs(ctx, ids)
	if err == nil && s.armed.CompareAndSwap(true, false) {
		if perr := s.PromoteToGlobal(ctx, "abc123", s.target); perr != nil {
			return nil, perr
		}
	}
	return mems, err
}

type notHistoryCapableRace struct {
	provider.MemoryStore
	inner *promoteAfterReadStore
}

func (s notHistoryCapableRace) GetByIDs(ctx context.Context, ids []string) ([]memory.Memory, error) {
	return s.inner.GetByIDs(ctx, ids)
}

func newOwnershipRaceSession(t *testing.T, historyCapable bool) (*memory.Store, *promoteAfterReadStore, *mcp.ClientSession, string) {
	t.Helper()
	ctx := context.Background()
	store := testStore(t)
	id, _, _, err := store.Upsert(ctx, "abc123", "fact", "the kvzr relay listens on the second port", "mcp", 0.5, nil)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	wrapped := &promoteAfterReadStore{Store: store, target: id}
	var ms provider.MemoryStore = wrapped
	if !historyCapable {
		ms = notHistoryCapableRace{MemoryStore: store, inner: wrapped}
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return store, wrapped, connectedClient(t, New(ms, logger, "test")), id
}

func historyLen(t *testing.T, store *memory.Store, id string) int {
	t.Helper()
	entries, err := store.MemoryHistory(context.Background(), id, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	return len(entries)
}

func rowOf(t *testing.T, store *memory.Store, id string) (memory.Memory, bool) {
	t.Helper()
	mems, err := store.GetByIDs(context.Background(), []string{id})
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	if len(mems) == 0 {
		return memory.Memory{}, false
	}
	return mems[0], true
}

func TestMemoryDeleteRefusesARowPromotedAfterTheOwnershipRead(t *testing.T) {
	cases := []struct {
		name           string
		purge          bool
		historyCapable bool
	}{
		{"plain delete", false, true},
		{"purge_history", true, true},
		{"purge_history without the history capability", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, wrapped, session, id := newOwnershipRaceSession(t, tc.historyCapable)
			before := historyLen(t, store, id)
			wrapped.armed.Store(true)

			out := callToolErr(t, session, "ghost_memory_delete", map[string]any{
				"project_id":    "test-project",
				"memory_id":     id,
				"purge_history": tc.purge,
			})
			if !strings.Contains(out, "not found in project") {
				t.Errorf("the refusal does not say the row left the project: %q", out)
			}
			if wrapped.armed.Load() {
				t.Fatal("the interleave never ran: the handler did not read the row before writing")
			}
			m, ok := rowOf(t, store, id)
			if !ok {
				t.Fatal("the global memory was deleted by a project-scoped call")
			}
			if m.ProjectID != memory.GlobalProjectID {
				t.Errorf("row project = %q, want %q", m.ProjectID, memory.GlobalProjectID)
			}
			if after := historyLen(t, store, id); after != before {
				t.Errorf("history rows %d -> %d: a refused delete wrote or erased history", before, after)
			}
		})
	}
}

func TestMemoryPinRefusesARowPromotedAfterTheOwnershipRead(t *testing.T) {
	store, wrapped, session, id := newOwnershipRaceSession(t, true)
	wrapped.armed.Store(true)

	out := callToolErr(t, session, "ghost_memory_pin", map[string]any{
		"project_id": "test-project",
		"memory_id":  id,
		"pinned":     true,
	})
	if !strings.Contains(out, "not found in project") {
		t.Errorf("the refusal does not say the row left the project: %q", out)
	}
	if wrapped.armed.Load() {
		t.Fatal("the interleave never ran: the handler did not read the row before writing")
	}
	m, ok := rowOf(t, store, id)
	if !ok {
		t.Fatal("the memory is gone")
	}
	if m.ProjectID != memory.GlobalProjectID || m.Pinned {
		t.Errorf("row = project %q pinned %v, want %q unpinned", m.ProjectID, m.Pinned, memory.GlobalProjectID)
	}
}

func TestMemoryDeleteAndPinStillWorkInTheirOwnProject(t *testing.T) {
	t.Run("pin and unpin", func(t *testing.T) {
		store, _, session, id := newOwnershipRaceSession(t, true)
		for _, want := range []bool{true, true, false} {
			res := callTool(t, session, "ghost_memory_pin", map[string]any{
				"project_id": "test-project", "memory_id": id, "pinned": want,
			})
			if res.IsError {
				t.Fatalf("pin %v: %s", want, resultText(res))
			}
			if m, _ := rowOf(t, store, id); m.Pinned != want {
				t.Errorf("pinned = %v, want %v", m.Pinned, want)
			}
		}
	})
	for _, tc := range []struct {
		name           string
		purge          bool
		historyCapable bool
	}{
		{"plain delete", false, true},
		{"purge_history", true, true},
		{"purge_history without the history capability", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, _, session, id := newOwnershipRaceSession(t, tc.historyCapable)
			res := callTool(t, session, "ghost_memory_delete", map[string]any{
				"project_id": "test-project", "memory_id": id, "purge_history": tc.purge,
			})
			if res.IsError {
				t.Fatalf("delete: %s", resultText(res))
			}
			if _, ok := rowOf(t, store, id); ok {
				t.Error("the memory survived its own project's delete")
			}
			if n := historyLen(t, store, id); tc.purge && n != 0 {
				t.Errorf("purge left %d history rows", n)
			} else if !tc.purge && n == 0 {
				t.Error("a plain delete left no tombstone")
			}
		})
	}
}
