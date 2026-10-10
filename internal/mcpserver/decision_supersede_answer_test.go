package mcpserver

import (
	"strings"
	"testing"
)

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

func TestDecisionRecordAnswerWarnsWhenTheOldMemoryIsAmbiguous(t *testing.T) {
	db, _, srv := newStoreWithDB(t)
	session := connectedClient(t, srv)
	first := callTool(t, session, "ghost_decision_record", map[string]any{
		"project_id": "abc123", "title": "t", "decision": "same", "rationale": "same",
	})
	if first.IsError {
		t.Fatal(resultText(first))
	}
	// Strip the link so the old decision is found by text, then add a second
	// decision with identical text: two claimants, nothing may be retired.
	if _, err := db.Exec(`UPDATE memories SET source_ref = NULL WHERE source = 'decision_log'`); err != nil {
		t.Fatal(err)
	}
	var oldID string
	if err := db.QueryRow(`SELECT id FROM decisions LIMIT 1`).Scan(&oldID); err != nil {
		t.Fatal(err)
	}
	plantDecision(t, db, "abc123", "DUP1", "t", "same", "same", "active")
	res := callTool(t, session, "ghost_decision_record", map[string]any{
		"project_id": "abc123", "title": "n", "decision": "new", "rationale": "new", "supersedes": oldID,
	})
	if res.IsError {
		t.Fatal(resultText(res))
	}
	if out := resultText(res); !strings.Contains(out, "more than one memory claims it") {
		t.Errorf("no ambiguity warning in the answer:\n%s", out)
	}
}
