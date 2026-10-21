package mcpserver

import (
	"strings"
	"testing"
)

// The reproduction from the board: a decision is recorded, its companion memory
// is edited with a source_ref of the caller's own, and the next decision
// supersedes the first.
//
// The edit used to REPLACE the link the companion carried, so the decision had
// no companion left: the supersede found nothing, answered a plain success with
// an empty Retirement, and the reversed decision kept ranking with a NULL
// resolved_at.
//
// The link is Ghost's to manage, so an edit that writes a reference keeps it.
func TestAMemoryUpdateKeepsTheDecisionLinkOnTheCompanion(t *testing.T) {
	db, _, srv := newStoreWithDB(t)
	session := connectedClient(t, srv)

	recorded := callTool(t, session, "ghost_decision_record", map[string]any{
		"project_id": "abc123",
		"title":      "Use Redis for the job queue",
		"decision":   "Redis lists as the queue backend",
		"rationale":  "Already deployed for caching",
	})
	if recorded.IsError {
		t.Fatalf("ghost_decision_record: %s", resultText(recorded))
	}
	decisionID := labeledID(t, resultText(recorded), "decision_id: ")
	companionID := labeledID(t, resultText(recorded), "memory_id: ")

	// The edit the decision tool itself points the caller at.
	updated := callTool(t, session, "ghost_memory_update", map[string]any{
		"project_id": "abc123",
		"memory_id":  companionID,
		"source_ref": "docs/queue.md",
	})
	if updated.IsError {
		t.Fatalf("ghost_memory_update: %s", resultText(updated))
	}

	var ref string
	if err := db.QueryRow(`SELECT COALESCE(source_ref, '') FROM memories WHERE id = ?`, companionID).Scan(&ref); err != nil {
		t.Fatalf("read the companion's source_ref: %v", err)
	}
	if !strings.Contains(ref, "decision:") {
		t.Fatalf("the companion's source_ref is %q, which carries no decision link: the edit dropped it", ref)
	}

	// The supersede that follows must retire the companion, not answer a clean
	// success while the reversed decision keeps ranking.
	superseded := callTool(t, session, "ghost_decision_record", map[string]any{
		"project_id": "abc123",
		"title":      "Reverse: use Postgres for the job queue",
		"decision":   "SKIP LOCKED on Postgres",
		"rationale":  "Redis lost jobs on failover",
		"supersedes": decisionID,
	})
	if superseded.IsError {
		t.Fatalf("ghost_decision_record (supersede): %s", resultText(superseded))
	}
	if out := resultText(superseded); !strings.Contains(out, "was retired") {
		t.Errorf("the supersede did not report the companion as retired:\n%s", out)
	}
	var resolved int
	if err := db.QueryRow(`SELECT count(*) FROM memories WHERE id = ? AND resolved_at IS NOT NULL`, companionID).Scan(&resolved); err != nil {
		t.Fatalf("read resolved_at: %v", err)
	}
	if resolved != 1 {
		t.Error("the companion is still live: search and session start keep answering with the reversed decision")
	}
}
