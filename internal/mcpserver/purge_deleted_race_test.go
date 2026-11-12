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
)

type restoreAfterHistoryReadStore struct {
	*memory.Store
	armed   atomic.Bool
	promote bool
	target  string
	hookErr error
}

func (s *restoreAfterHistoryReadStore) MemoryHistory(ctx context.Context, id string, limit int) ([]memory.HistoryEntry, error) {
	entries, err := s.Store.MemoryHistory(ctx, id, limit)
	if err == nil && s.armed.CompareAndSwap(true, false) {
		if _, rerr := s.RestoreSnapshot(ctx, "abc123"); rerr != nil {
			s.hookErr = rerr
		} else if s.promote {
			s.hookErr = s.PromoteToGlobal(ctx, "abc123", s.target)
		}
	}
	return entries, err
}

const purgeRaceText = "the staging api token lives in vault path ops/stg"

func newPurgeDeletedRaceSession(t *testing.T, promote bool) (*memory.Store, *restoreAfterHistoryReadStore, *mcp.ClientSession, string) {
	t.Helper()
	ctx := context.Background()
	store := testStore(t)
	id, _, _, err := store.Upsert(ctx, "abc123", "fact", purgeRaceText, "mcp", 0.5, nil)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if _, _, _, _, err := store.ReplaceNonManual(ctx, "abc123", []memory.Memory{{Category: "fact", Content: "a consolidated note about staging", Importance: 0.5}}, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}
	if _, ok := rowOf(t, store, id); ok {
		t.Fatal("fixture: the reflection did not delete the memory")
	}
	w := &restoreAfterHistoryReadStore{Store: store, promote: promote, target: id}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return store, w, connectedClient(t, New(w, logger, "test")), id
}

func TestMemoryDeletePurgeOfADeletedMemoryRefusesARowThatCameBack(t *testing.T) {
	cases := []struct {
		name        string
		promote     bool
		wantProject string
		wantRefusal string
	}{
		{"restored", false, "abc123", "live again"},
		{"restored and promoted to _global", true, memory.GlobalProjectID, "not found in project"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, w, session, id := newPurgeDeletedRaceSession(t, tc.promote)
			w.armed.Store(true)

			out := callToolErr(t, session, "ghost_memory_delete", map[string]any{
				"project_id":    "test-project",
				"memory_id":     id,
				"purge_history": true,
			})
			if w.hookErr != nil {
				t.Fatalf("interleave: %v", w.hookErr)
			}
			if w.armed.Load() {
				t.Fatal("the interleave never ran: the handler did not read the history before purging")
			}
			if strings.Contains(out, "already deleted") {
				t.Errorf("the refusal still reports the memory as already deleted: %q", out)
			}
			if !strings.Contains(out, tc.wantRefusal) {
				t.Errorf("refusal = %q, want it to contain %q", out, tc.wantRefusal)
			}
			m, live := rowOf(t, store, id)
			if !live {
				t.Fatal("the restored memory is gone after a refused purge")
			}
			if m.ProjectID != tc.wantProject || m.Content != purgeRaceText {
				t.Errorf("row = project %q content %q, want %q %q", m.ProjectID, m.Content, tc.wantProject, purgeRaceText)
			}
			if n := historyLen(t, store, id); n == 0 {
				t.Error("a refused purge erased the memory's history")
			}
		})
	}
}
