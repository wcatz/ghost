package mcpserver

import "testing"

// The superseded decision's id reaches the answer only as a token, and a
// decision_log memory cannot be forged through the writer tools.
func TestDecisionRecordAnswerTokenisesTheSupersededID(t *testing.T) {
	db, _, srv := newStoreWithDB(t)
	session := connectedClient(t, srv)
	id := hostileIDFor()
	plantDecision(t, db, "abc123", id, "old", "old decision", "old rationale", "active")

	res := callTool(t, session, "ghost_decision_record", map[string]any{
		"project_id": "abc123",
		"title":      "new",
		"decision":   "new decision",
		"rationale":  "new rationale",
		"supersedes": id,
	})
	if res.IsError {
		t.Fatalf("ghost_decision_record failed: %s", resultText(res))
	}
	assertTheIDIsOnlyAToken(t, "ghost_decision_record answer", resultText(res), id)
}

func TestMemorySaveRefusesTheReservedDecisionRef(t *testing.T) {
	_, _, srv := newStoreWithDB(t)
	session := connectedClient(t, srv)
	res := callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": "abc123",
		"content":    "an unrelated memory",
		"category":   "fact",
		"source_ref": "decision:abc",
	})
	if !res.IsError {
		t.Errorf("ghost_memory_save accepted the reserved source_ref: %s", resultText(res))
	}
}
