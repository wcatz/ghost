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
	if !res.IsError {
		t.Errorf("an ambiguous retirement was reported as a plain success; a caller checking only isError "+
			"reads the reversal as fully done:\n%s", resultText(res))
	}
	if out := resultText(res); !strings.Contains(out, "more than one memory claims it") {
		t.Errorf("no ambiguity warning in the answer:\n%s", out)
	}
}

// A decision token anywhere in the reference is refused by every writer tool,
// because the decision lookup reads every comma-separated token.
func TestWriterToolsRefuseAnEmbeddedDecisionRef(t *testing.T) {
	_, _, srv := newStoreWithDB(t)
	session := connectedClient(t, srv)
	const ref = "runbook.md,decision:abc"
	for _, tool := range []string{"ghost_memory_save", "ghost_save_global"} {
		args := map[string]any{"content": "an unrelated memory via " + tool, "category": "decision", "source_ref": ref}
		if tool == "ghost_memory_save" {
			args["project_id"] = "abc123"
		}
		if res := callTool(t, session, tool, args); !res.IsError {
			t.Errorf("%s accepted %q: %s", tool, ref, resultText(res))
		}
	}
	saved := callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": "abc123", "content": "a plain memory", "category": "decision",
	})
	if saved.IsError {
		t.Fatal(resultText(saved))
	}
	id := labeledID(t, resultText(saved), "(id: ")
	if res := callTool(t, session, "ghost_memory_update", map[string]any{
		"project_id": "abc123", "memory_id": id, "source_ref": ref,
	}); !res.IsError {
		t.Errorf("ghost_memory_update accepted %q: %s", ref, resultText(res))
	}
}

// A merged memory that holds the reversed decision together with another one is
// not an extra claimant. The answer still reports a partial failure (the reversed
// text keeps ranking), but it must not tell the agent to delete that memory: it
// is the only memory of a decision that is still live.
func TestDecisionRecordAnswerReportsASharedSuccessorWithoutDeleteAdvice(t *testing.T) {
	db, _, srv := newStoreWithDB(t)
	session := connectedClient(t, srv)
	record := func(title string) (string, string) {
		res := callTool(t, session, "ghost_decision_record", map[string]any{
			"project_id": "abc123", "title": title, "decision": title + " decision", "rationale": title + " rationale",
		})
		if res.IsError {
			t.Fatal(resultText(res))
		}
		return labeledID(t, resultText(res), "decision_id: "), labeledID(t, resultText(res), "memory_id: ")
	}
	aDec, aMem := record("backend")
	bDec, bMem := record("retries")
	// What a consolidation leaves behind after folding both companions: one
	// reflection row carrying both links, and neither original.
	if _, err := db.Exec(`UPDATE memories SET source = 'reflection', source_ref = ? WHERE id = ?`,
		"decision:"+aDec+",decision:"+bDec, aMem); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM memories WHERE id = ?`, bMem); err != nil {
		t.Fatal(err)
	}
	res := callTool(t, session, "ghost_decision_record", map[string]any{
		"project_id": "abc123", "title": "backend v2", "decision": "use SQS", "rationale": "managed", "supersedes": aDec,
	})
	out := resultText(res)
	if !res.IsError {
		t.Errorf("a shared successor left live was reported as a plain success:\n%s", out)
	}
	if !strings.Contains(out, aMem) {
		t.Errorf("the answer does not name the shared memory %s:\n%s", aMem, out)
	}
	if strings.Contains(out, "delete") || strings.Contains(out, "more than one memory claims it") {
		t.Errorf("the answer treats the shared memory as an extra claimant and advises deleting it:\n%s", out)
	}
}

// Two live memories claim the reversed decision: the companion the decision
// tool wrote and a consolidation merge that also holds another memory's
// content. The answer must not tell the agent to delete "the extras" — the
// merge is the only copy of the fact it folded in — nor to supersede again,
// which the tool refuses once the decision is marked superseded. Following the
// advice it does give leaves no live memory stating the reversed decision.
func TestDecisionRecordAnswerGivesAmbiguousClaimantsAdviceThatWorks(t *testing.T) {
	db, _, srv := newStoreWithDB(t)
	session := connectedClient(t, srv)
	first := callTool(t, session, "ghost_decision_record", map[string]any{
		"project_id": "abc123", "title": "queue", "decision": "we use nats for the event queue", "rationale": "it was cheapest",
	})
	if first.IsError {
		t.Fatal(resultText(first))
	}
	oldDec := labeledID(t, resultText(first), "decision_id: ")
	comp := labeledID(t, resultText(first), "memory_id: ")
	saved := callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": "abc123", "category": "fact",
		"content": "the event queue runs on nats, and its consumer restarts in place",
	})
	if saved.IsError {
		t.Fatal(resultText(saved))
	}
	merge := labeledID(t, resultText(saved), "(id: ")
	// What a reflect merge of the companion and the fact leaves when the
	// companion's row is also kept live: the merge carries the link too.
	if _, err := db.Exec(`UPDATE memories SET source = 'reflection', category = 'decision', source_ref = ? WHERE id = ?`,
		"decision:"+oldDec, merge); err != nil {
		t.Fatal(err)
	}

	res := callTool(t, session, "ghost_decision_record", map[string]any{
		"project_id": "abc123", "title": "queue v2", "decision": "we use kafka for the event queue", "rationale": "scale", "supersedes": oldDec,
	})
	out := resultText(res)
	if !res.IsError {
		t.Errorf("an ambiguous retirement was reported as a plain success:\n%s", out)
	}
	for _, want := range []string{comp, merge, "more than one memory claims it", "no second supersede", "ghost_memory_update", "ghost_memory_delete"} {
		if !strings.Contains(out, want) {
			t.Errorf("the answer lacks %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{"delete the extras", "supersede again"} {
		if strings.Contains(out, bad) {
			t.Errorf("the answer still advises %q:\n%s", bad, out)
		}
	}

	// The advice says there is no second supersede to run: the tool refuses one.
	again := callTool(t, session, "ghost_decision_record", map[string]any{
		"project_id": "abc123", "title": "queue v3", "decision": "we use kafka", "rationale": "again", "supersedes": oldDec,
	})
	if !again.IsError || !strings.Contains(resultText(again), "already superseded") {
		t.Errorf("a second supersede of the same decision was not refused:\n%s", resultText(again))
	}

	// Following the advice: the merge holds the fact, so it is rewritten; the
	// companion holds nothing else, so it is deleted.
	if upd := callTool(t, session, "ghost_memory_update", map[string]any{
		"project_id": "abc123", "memory_id": merge, "content": "the event queue consumer restarts in place",
	}); upd.IsError {
		t.Fatalf("ghost_memory_update: %s", resultText(upd))
	}
	if del := callTool(t, session, "ghost_memory_delete", map[string]any{"project_id": "abc123", "memory_id": comp}); del.IsError {
		t.Fatalf("ghost_memory_delete: %s", resultText(del))
	}
	var stating int
	if err := db.QueryRow(`SELECT count(*) FROM memories WHERE project_id = 'abc123' AND resolved_at IS NULL AND lower(content) LIKE '%nats%'`).Scan(&stating); err != nil {
		t.Fatal(err)
	}
	if stating != 0 {
		t.Errorf("%d live memories still state the reversed decision after following the advice", stating)
	}
	var fact int
	if err := db.QueryRow(`SELECT count(*) FROM memories WHERE id = ? AND content LIKE '%restarts in place%'`, merge).Scan(&fact); err != nil {
		t.Fatal(err)
	}
	if fact != 1 {
		t.Error("following the advice lost the fact the merge held")
	}
}
