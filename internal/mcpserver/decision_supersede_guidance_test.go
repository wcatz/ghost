package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The rule that a changed decision is recorded with supersedes is told to the
// agent on both surfaces it reads: the tool description and the server
// instructions.
func TestChangedDecisionGuidanceIsOnBothSurfaces(t *testing.T) {
	const rule = "call ghost_decisions_list to find it and record the new decision"
	if !strings.Contains(mcpInstructions, rule) || !strings.Contains(mcpInstructions, "supersedes=<that decision_id>") ||
		!strings.Contains(mcpInstructions, "never a second unlinked decision") {
		t.Errorf("mcpInstructions does not tell the agent to supersede a changed decision")
	}
	_, _, srv := newStoreWithDB(t)
	session := connectedClient(t, srv)
	tools, err := session.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name != "ghost_decision_record" {
			continue
		}
		if !strings.Contains(tool.Description, rule) || !strings.Contains(tool.Description, "supersedes=<that decision_id>") ||
			!strings.Contains(tool.Description, "never a second unlinked decision") {
			t.Errorf("ghost_decision_record description does not tell the agent to supersede a changed decision:\n%s", tool.Description)
		}
		return
	}
	t.Fatal("ghost_decision_record is not listed")
}

func recordTitled(t *testing.T, session *mcp.ClientSession, project, title string, extra map[string]any) string {
	t.Helper()
	args := map[string]any{"project_id": project, "title": title, "decision": "d", "rationale": "r"}
	for k, v := range extra {
		args[k] = v
	}
	res := callTool(t, session, "ghost_decision_record", args)
	if res.IsError {
		t.Fatalf("ghost_decision_record failed: %s", resultText(res))
	}
	return resultText(res)
}

func TestDecisionRecordAdvisesSupersedesOnASameTitleLiveDecision(t *testing.T) {
	db, _, srv := newStoreWithDB(t)
	session := connectedClient(t, srv)
	plantDecision(t, db, "abc123", "OLD1", " Max 5 attempts ", "5", "r", "active")

	out := recordTitled(t, session, "abc123", "  max 5 ATTEMPTS ", nil)
	if !strings.Contains(out, "OLD1") || !strings.Contains(out, "supersedes=OLD1") {
		t.Errorf("no advice naming the live decision:\n%s", out)
	}
	if !strings.Contains(out, "Decision recorded") {
		t.Errorf("the advice must not stop the record:\n%s", out)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM decisions WHERE id = 'OLD1'`).Scan(&status); err != nil || status != "active" {
		t.Errorf("the old decision was changed by advice: status=%q err=%v", status, err)
	}
}

func TestDecisionRecordGivesNoAdviceWithoutAMatch(t *testing.T) {
	db, _, srv := newStoreWithDB(t)
	session := connectedClient(t, srv)
	plantDecision(t, db, "abc123", "OLD1", "Max 5 attempts", "5", "r", "active")

	if out := recordTitled(t, session, "abc123", "Use SQLite", nil); strings.Contains(out, "pass supersedes") {
		t.Errorf("advice on a title that matches nothing:\n%s", out)
	}
	// A first decision does not match itself.
	if out := recordTitled(t, session, "abc123", "Fresh title", nil); strings.Contains(out, "pass supersedes") {
		t.Errorf("a new decision matched itself:\n%s", out)
	}
}

func TestDecisionRecordIgnoresASupersededOrOtherProjectMatch(t *testing.T) {
	db, _, srv := newStoreWithDB(t)
	session := connectedClient(t, srv)
	plantDecision(t, db, "abc123", "GONE1", "Max 5 attempts", "5", "r", "superseded")
	plantProject(t, db, "other", "other", "/p/other")
	plantDecision(t, db, "other", "ELSE1", "Max 5 attempts", "5", "r", "active")

	if out := recordTitled(t, session, "abc123", "Max 5 attempts", nil); strings.Contains(out, "supersedes=") || strings.Contains(out, "GONE1") || strings.Contains(out, "ELSE1") {
		t.Errorf("advice named a superseded or other-project decision:\n%s", out)
	}
}

func TestDecisionRecordGivesNoAdviceWhenSupersedesIsPassed(t *testing.T) {
	db, _, srv := newStoreWithDB(t)
	session := connectedClient(t, srv)
	plantDecision(t, db, "abc123", "OLD1", "Max 5 attempts", "5", "r", "active")

	out := recordTitled(t, session, "abc123", "Max 5 attempts", map[string]any{"supersedes": "OLD1"})
	if strings.Contains(out, "NOTE:") {
		t.Errorf("advice given although supersedes was passed:\n%s", out)
	}
}

// A decision flagged for revisit is live, not superseded, so it draws the advice.
func TestDecisionRecordAdvisesSupersedesOnARevisitDecision(t *testing.T) {
	db, _, srv := newStoreWithDB(t)
	session := connectedClient(t, srv)
	plantDecision(t, db, "abc123", "REV1", "Max 5 attempts", "5", "r", "revisit")

	if out := recordTitled(t, session, "abc123", "Max 5 attempts", nil); !strings.Contains(out, "supersedes=REV1") {
		t.Errorf("no advice naming the revisit decision:\n%s", out)
	}
}
