package mcpserver

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/provider"
)

// recordDecisionID records one decision through the tool and returns its
// decision_id, failing the test on an error result.
func recordDecisionID(t *testing.T, session *mcp.ClientSession, args map[string]any) string {
	t.Helper()
	res := callTool(t, session, "ghost_decision_record", args)
	if res.IsError {
		t.Fatalf("record %v failed: %s", args["title"], resultText(res))
	}
	return labeledID(t, resultText(res), "decision_id: ")
}

// decisionPointer reads one decision's status and superseded_by.
func decisionPointer(t *testing.T, db *sql.DB, id string) (status, supersededBy string) {
	t.Helper()
	var by sql.NullString
	if err := db.QueryRow(`SELECT status, superseded_by FROM decisions WHERE id = ?`, id).Scan(&status, &by); err != nil {
		t.Fatalf("read decision %s: %v", id, err)
	}
	return status, by.String
}

func countRows(t *testing.T, db *sql.DB, query string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// TestDecisionSupersedeOfAnAlreadyReplacedDecisionIsRefused is #1144: A was
// replaced by B, and a later record with supersedes=A used to answer "Decision
// A is now marked superseded by this one." as a plain success while it
// overwrote A's superseded_by from B to C. B and C were then both active, both
// companion memories stayed live, and nothing could say B ever replaced A.
//
// The record is refused before anything is written: no C row, no companion
// memory, A still points at B and B stays active. The answer names B and tells
// the caller to pass supersedes=B, which is the decision that is current.
func TestDecisionSupersedeOfAnAlreadyReplacedDecisionIsRefused(t *testing.T) {
	db, session, _ := echoLogSession(t)
	a := recordDecisionID(t, session, map[string]any{
		"project_id": "p", "title": "queue backend", "decision": "use Redis", "rationale": "deployed",
	})
	b := recordDecisionID(t, session, map[string]any{
		"project_id": "p", "title": "queue backend", "decision": "use Postgres", "rationale": "Redis lost jobs",
		"supersedes": a,
	})
	decisionsBefore := countRows(t, db, `SELECT count(*) FROM decisions`)
	memoriesBefore := countRows(t, db, `SELECT count(*) FROM memories`)

	res := callTool(t, session, "ghost_decision_record", map[string]any{
		"project_id": "p", "title": "queue backend", "decision": "use SQS", "rationale": "managed",
		"supersedes": a,
	})
	out := resultText(res)
	if !res.IsError {
		t.Fatalf("superseding a decision that already has a live successor was reported as a success:\n%s", out)
	}
	if !strings.Contains(out, b) {
		t.Errorf("the refusal does not name the live successor %s:\n%s", b, out)
	}
	if !strings.Contains(out, "supersedes="+b) {
		t.Errorf("the refusal does not suggest supersedes=%s:\n%s", b, out)
	}
	if strings.Contains(out, "decision_id: ") {
		t.Errorf("the refusal reports a committed decision, but nothing should have been written:\n%s", out)
	}

	if n := countRows(t, db, `SELECT count(*) FROM decisions`); n != decisionsBefore {
		t.Errorf("decisions = %d after the refusal, want %d: the refused decision was written", n, decisionsBefore)
	}
	if n := countRows(t, db, `SELECT count(*) FROM memories`); n != memoriesBefore {
		t.Errorf("memories = %d after the refusal, want %d: the refused decision's companion was written", n, memoriesBefore)
	}
	if status, by := decisionPointer(t, db, a); status != "superseded" || by != b {
		t.Errorf("A is %s superseded_by %q, want superseded by %s", status, by, b)
	}
	if status, _ := decisionPointer(t, db, b); status != "active" {
		t.Errorf("B is %s, want active", status)
	}
}

// TestDecisionSupersedeRepointsWhenTheSuccessorWasItselfReplaced is the other
// side of the #1144 rule: A was replaced by B and B was later replaced by D, so
// A has no live successor left to lose. Pointing A at a new decision C is then a
// repoint of a dead link, not the loss of a live one, and it proceeds as before.
func TestDecisionSupersedeRepointsWhenTheSuccessorWasItselfReplaced(t *testing.T) {
	db, session, _ := echoLogSession(t)
	a := recordDecisionID(t, session, map[string]any{
		"project_id": "p", "title": "queue backend", "decision": "use Redis", "rationale": "deployed",
	})
	b := recordDecisionID(t, session, map[string]any{
		"project_id": "p", "title": "queue backend v2", "decision": "use Postgres", "rationale": "Redis lost jobs",
		"supersedes": a,
	})
	recordDecisionID(t, session, map[string]any{
		"project_id": "p", "title": "queue backend v3", "decision": "use Kafka", "rationale": "volume",
		"supersedes": b,
	})

	res := callTool(t, session, "ghost_decision_record", map[string]any{
		"project_id": "p", "title": "queue backend v4", "decision": "use SQS", "rationale": "managed",
		"supersedes": a,
	})
	out := resultText(res)
	if res.IsError {
		t.Fatalf("superseding a decision whose successor was itself replaced was refused:\n%s", out)
	}
	c := labeledID(t, out, "decision_id: ")
	if status, by := decisionPointer(t, db, a); status != "superseded" || by != c {
		t.Errorf("A is %s superseded_by %q, want superseded by %s", status, by, c)
	}
}

// staleSuccessorStore answers the handler's read-only successor check with
// "none", as a store would when a second writer superseded the target between
// that read and the supersede itself. Everything else is the real store, so the
// supersede's own in-transaction guard is what has to catch it.
type staleSuccessorStore struct{ provider.MemoryStore }

func (staleSuccessorStore) LiveDecisionSuccessor(context.Context, string, string) (string, error) {
	return "", nil
}

// TestDecisionSupersedeRaceKeepsTheNewDecisionAndDoesNotSendItRoundAgain is the
// backstop behind the #1144 refusal: when the read before the write saw no live
// successor but the supersede's transaction does, the new decision is already
// committed. The answer is the #1041 partial failure, and it must not tell the
// caller to record again with supersedes=<successor> — that would write a
// second copy of the decision that just landed.
func TestDecisionSupersedeRaceKeepsTheNewDecisionAndDoesNotSendItRoundAgain(t *testing.T) {
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	session := connectedClient(t, New(staleSuccessorStore{memory.NewStore(db, logger)}, logger, "test"))

	a := recordDecisionID(t, session, map[string]any{
		"project_id": "p", "title": "queue backend", "decision": "use Redis", "rationale": "deployed",
	})
	b := recordDecisionID(t, session, map[string]any{
		"project_id": "p", "title": "queue backend", "decision": "use Postgres", "rationale": "Redis lost jobs",
		"supersedes": a,
	})

	res := callTool(t, session, "ghost_decision_record", map[string]any{
		"project_id": "p", "title": "queue backend", "decision": "use SQS", "rationale": "managed",
		"supersedes": a,
	})
	out := resultText(res)
	if !res.IsError {
		t.Fatalf("a supersede the transaction refused was reported as a success:\n%s", out)
	}
	c := labeledID(t, out, "decision_id: ")
	labeledID(t, out, "memory_id: ")
	if !strings.Contains(out, a) || !strings.Contains(out, b) {
		t.Errorf("the answer does not name both the target %s and its live successor %s:\n%s", a, b, out)
	}
	if !strings.Contains(out, "NOT linked") {
		t.Errorf("the answer does not say the new decision was left unlinked:\n%s", out)
	}
	if strings.Contains(out, "supersedes="+b) || strings.Contains(out, "supersede that one instead") {
		t.Errorf("the answer tells the caller to record the committed decision again:\n%s", out)
	}
	if status, by := decisionPointer(t, db, a); status != "superseded" || by != b {
		t.Errorf("A is %s superseded_by %q, want superseded by %s", status, by, b)
	}
	if status, _ := decisionPointer(t, db, c); status != "active" {
		t.Errorf("the committed decision %s is %s, want active", c, status)
	}
}
