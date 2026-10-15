package mcpserver

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// flagCountUnavailableStore records flags normally and fails only on the
// read-back, so ghost_memory_flag reaches its "was flagged, but the count could
// not be read back" sentence — the one answer on that tool which still has to
// name the id the caller addressed.
type flagCountUnavailableStore struct {
	*memory.Store
}

func (s *flagCountUnavailableStore) UsefulnessByMemory(context.Context, string) (map[string]memory.UsefulnessEvidence, error) {
	return nil, errors.New("counts are unavailable")
}

// TestToolResultsRenderAStoredMemoryIDThroughTheSafeToken is #1377 over the
// tools whose answers name a stored memory id.
//
// The id is planted in SQL, like every other hostile id in this package, because
// ImportMemory refuses one carrying a control character since #791 — and the
// rows these tools must survive are exactly the ones a rejected writer never
// made: an artifact imported before the refusal, a restored snapshot, a hand
// edit. A raw print lets the row forge extra lines in the answer the model
// reads, so the value has to reach the reader only through assemble.Token.
//
// The four tools the audit named are here, and so are the ones sharing their
// shape: ghost_memory_promote's success line and the already-deleted purge
// report print the same addressed id, and the global save folds the same way the
// project save does. Each case requires the tool to have SUCCEEDED and named the
// id at all, so a case cannot pass by taking an early refusal.
func TestToolResultsRenderAStoredMemoryIDThroughTheSafeToken(t *testing.T) {
	const note = "a planted note about the deploy ordering"

	t.Run("ghost_memory_update", func(t *testing.T) {
		db, _, srv := newStoreWithDB(t)
		id := hostileIDFor()
		plantMemoryWithID(t, db, "abc123", id, "fact", note)

		session := connectedClient(t, srv)
		res := callTool(t, session, "ghost_memory_update", map[string]any{
			"project_id": "test-project", "memory_id": id, "importance": 0.7,
		})
		if res.IsError {
			t.Fatalf("update refused the planted row: %s", resultText(res))
		}
		assertTheIDIsOnlyAToken(t, "ghost_memory_update", resultText(res), id)
	})

	t.Run("ghost_memory_promote", func(t *testing.T) {
		db, _, srv := newStoreWithDB(t)
		id := hostileIDFor()
		plantMemoryWithID(t, db, "abc123", id, "fact", note)

		session := connectedClient(t, srv)
		res := callTool(t, session, "ghost_memory_promote", map[string]any{
			"project_id": "test-project", "memory_id": id,
		})
		if res.IsError {
			t.Fatalf("promote refused the planted row: %s", resultText(res))
		}
		assertTheIDIsOnlyAToken(t, "ghost_memory_promote", resultText(res), id)
	})

	t.Run("ghost_memory_delete purge_history", func(t *testing.T) {
		db, _, srv := newStoreWithDB(t)
		id := hostileIDFor()
		plantMemoryWithID(t, db, "abc123", id, "fact", note)

		session := connectedClient(t, srv)
		res := callTool(t, session, "ghost_memory_delete", map[string]any{
			"project_id": "test-project", "memory_id": id, "purge_history": true,
		})
		if res.IsError {
			t.Fatalf("delete refused the planted row: %s", resultText(res))
		}
		assertTheIDIsOnlyAToken(t, "ghost_memory_delete", resultText(res), id)
	})

	t.Run("ghost_memory_save fold", func(t *testing.T) {
		db, _, srv := newStoreWithDB(t)
		id := hostileIDFor()
		plantMemoryWithID(t, db, "abc123", id, "fact", note)

		session := connectedClient(t, srv)
		res := callTool(t, session, "ghost_memory_save", map[string]any{
			"project_id": "test-project", "content": note, "category": "fact",
		})
		if res.IsError {
			t.Fatalf("save refused: %s", resultText(res))
		}
		out := resultText(res)
		if !strings.Contains(out, "linked as a likely duplicate of ") {
			t.Fatalf("the save did not fold into the planted row, so no stored id was named: %s", out)
		}
		assertTheIDIsOnlyAToken(t, "ghost_memory_save", out, id)
	})

	t.Run("ghost_save_global fold", func(t *testing.T) {
		db, store, srv := newStoreWithDB(t)
		id := hostileIDFor()
		ctx := context.Background()
		if err := store.EnsureProject(ctx, memory.GlobalProjectID, "global", "global"); err != nil {
			t.Fatalf("EnsureProject(_global): %v", err)
		}
		plantMemoryWithID(t, db, memory.GlobalProjectID, id, "fact", note)

		session := connectedClient(t, srv)
		res := callTool(t, session, "ghost_save_global", map[string]any{
			"content": note, "category": "fact",
		})
		if res.IsError {
			t.Fatalf("global save refused: %s", resultText(res))
		}
		out := resultText(res)
		if !strings.Contains(out, "linked as a likely duplicate of ") {
			t.Fatalf("the global save did not fold into the planted row, so no stored id was named: %s", out)
		}
		assertTheIDIsOnlyAToken(t, "ghost_save_global", out, id)
	})

	t.Run("ghost_memory_flag read-back failure", func(t *testing.T) {
		db, store, _ := newStoreWithDB(t)
		id := hostileIDFor()
		plantMemoryWithID(t, db, "abc123", id, "fact", note)

		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
		srv := New(&flagCountUnavailableStore{Store: store}, logger, "test")
		session := connectedClient(t, srv)
		out := callToolErr(t, session, "ghost_memory_flag", map[string]any{
			"project_id": "test-project", "memory_id": id, "kind": "stale",
			"reason": "the deploy order changed",
		})
		if !strings.Contains(out, "was flagged") {
			t.Fatalf("the flag did not reach the read-back failure: %.400q", out)
		}
		assertTheIDIsOnlyAToken(t, "ghost_memory_flag", out, id)
	})
}
