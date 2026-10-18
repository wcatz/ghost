package mcpserver

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/provider"
)

// cancelAfterRecordStore is the real store with the request cancelled right
// after the decision is written: every store call the handler makes after
// RecordDecision returns runs on a cancelled context, as it would when the
// host cancels the call on a timeout in that window.
type cancelAfterRecordStore struct {
	provider.MemoryStore
	armed    bool
	recorded bool
}

func cancelledFrom(ctx context.Context) context.Context {
	c, cancel := context.WithCancel(ctx)
	cancel()
	return c
}

func (s *cancelAfterRecordStore) RecordDecision(ctx context.Context, projectID, title, decision, rationale string, alternatives, tags []string) (string, string, bool, error) {
	d, m, cut, err := s.MemoryStore.RecordDecision(ctx, projectID, title, decision, rationale, alternatives, tags)
	if s.armed {
		s.recorded = true
	}
	return d, m, cut, err
}

func (s *cancelAfterRecordStore) SupersedeDecisionReport(ctx context.Context, projectID, oldID, newID string) (memory.DecisionRetirement, error) {
	if s.recorded {
		ctx = cancelledFrom(ctx)
	}
	return s.MemoryStore.SupersedeDecisionReport(ctx, projectID, oldID, newID)
}

// RecordDecisionSuperseding is the one write the handler makes now; armed, it
// runs on a cancelled context. The cancel landing INSIDE that transaction,
// after the insert, is TestRecordDecisionSupersedingCancelledAfterTheInsert
// in internal/memory, which can reach the window this wrapper cannot.
func (s *cancelAfterRecordStore) RecordDecisionSuperseding(ctx context.Context, projectID, title, decision, rationale string, alternatives, tags []string, oldID string) (memory.RecordedDecision, error) {
	if s.armed {
		ctx = cancelledFrom(ctx)
	}
	return s.MemoryStore.RecordDecisionSuperseding(ctx, projectID, title, decision, rationale, alternatives, tags, oldID)
}

// TestADecisionRecordCancelledBeforeItsSupersedeLeavesNoHalfWrite is the
// cancel between a decision and the supersede it carries. The two used to be
// separate writes on the request context, so a cancel in between left the new
// decision live beside the one it replaces, and a retry of the same call then
// answered a clean success with a second live copy.
//
// The record and the supersede are one write now: a cancelled call leaves
// the store as it found it, and the retry is the only write.
func TestADecisionRecordCancelledBeforeItsSupersedeLeavesNoHalfWrite(t *testing.T) {
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := &cancelAfterRecordStore{MemoryStore: memory.NewStore(db, logger)}
	session := connectedClient(t, New(store, logger, "test"))

	a := recordDecisionID(t, session, map[string]any{
		"project_id": "p", "title": "storage", "decision": "Use A", "rationale": "first",
	})
	decisionsBefore := countRows(t, db, `SELECT count(*) FROM decisions`)
	memoriesBefore := countRows(t, db, `SELECT count(*) FROM memories`)
	historyBefore := countRows(t, db, `SELECT count(*) FROM memory_history`)

	store.armed = true
	args := map[string]any{
		"project_id": "p", "title": "storage", "decision": "Use B", "rationale": "A failed",
		"supersedes": a,
	}
	// The cancelled call is not a success, and it must not have half-written.
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "ghost_decision_record", Arguments: args})
	if err == nil && !res.IsError {
		t.Errorf("the cancelled call answered a success:\n%s", resultText(res))
	}
	store.armed, store.recorded = false, false

	if status, _ := decisionPointer(t, db, a); status != "active" {
		t.Errorf("A is %s after the cancelled call, want active: the call wrote nothing, so nothing was superseded", status)
	}
	if n := countRows(t, db, `SELECT count(*) FROM decisions`); n != decisionsBefore {
		t.Errorf("decisions = %d after the cancelled call, want %d: the new decision committed without its supersede", n, decisionsBefore)
	}
	if n := countRows(t, db, `SELECT count(*) FROM memories`); n != memoriesBefore {
		t.Errorf("memories = %d after the cancelled call, want %d", n, memoriesBefore)
	}
	if n := countRows(t, db, `SELECT count(*) FROM memory_history`); n != historyBefore {
		t.Errorf("memory_history = %d after the cancelled call, want %d", n, historyBefore)
	}

	// The retry is the one write: exactly one live "Use B", and A points at it.
	b := recordDecisionID(t, session, args)
	if n := countRows(t, db, `SELECT count(*) FROM decisions WHERE decision = 'Use B' AND status != 'superseded'`); n != 1 {
		t.Errorf("live 'Use B' decisions after the retry = %d, want 1", n)
	}
	if status, by := decisionPointer(t, db, a); status != "superseded" || by != b {
		t.Errorf("A is %s superseded_by %q after the retry, want superseded by %s", status, by, b)
	}
}
