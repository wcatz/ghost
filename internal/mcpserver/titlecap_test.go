package mcpserver

import (
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func newTitleCapSession(t *testing.T) *mcp.ClientSession {
	t.Helper()
	srv, session := newCapSession(t)
	_ = srv
	return session
}

func TestTaskCreateWarnsOnTruncatedTitle(t *testing.T) {
	session := newTitleCapSession(t)
	res := callTool(t, session, "ghost_task_create", map[string]any{
		"project_id": "test-project",
		"title":      strings.Repeat("T", 301),
	})
	resp := resultText(res)
	if !strings.Contains(resp, "task title was truncated at 300 bytes") {
		t.Fatalf("over-cap title must warn: %q", resp)
	}

	session2 := newTitleCapSession(t)
	res2 := callTool(t, session2, "ghost_task_create", map[string]any{
		"project_id": "test-project",
		"title":      strings.Repeat("T", 300),
	})
	resp2 := resultText(res2)
	if strings.Contains(resp2, "truncated") {
		t.Fatalf("exactly-cap title must not warn: %q", resp2)
	}
}

func TestDecisionRecordWarnsOnTruncatedTitle(t *testing.T) {
	session := newTitleCapSession(t)
	res := callTool(t, session, "ghost_decision_record", map[string]any{
		"project_id": "test-project",
		"title":      strings.Repeat("T", 301),
		"decision":   "decision",
		"rationale":  "rationale",
	})
	resp := resultText(res)
	if !strings.Contains(resp, "decision title was truncated at 300 bytes") {
		t.Fatalf("over-cap decision title must warn: %q", resp)
	}

	session2 := newTitleCapSession(t)
	res2 := callTool(t, session2, "ghost_decision_record", map[string]any{
		"project_id": "test-project",
		"title":      strings.Repeat("T", 300),
		"decision":   "decision",
		"rationale":  "rationale",
	})
	resp2 := resultText(res2)
	if strings.Contains(resp2, "decision title was truncated") {
		t.Fatalf("uncut decision title must not warn: %q", resp2)
	}
}

func TestDecisionRecordWarnsOnTruncatedAlternatives(t *testing.T) {
	session := newTitleCapSession(t)
	alts := make([]string, 21)
	for i := range alts {
		alts[i] = "alternative"
	}
	res := callTool(t, session, "ghost_decision_record", map[string]any{
		"project_id":   "test-project",
		"title":        "decision with too many alternatives",
		"decision":     "decision",
		"rationale":    "rationale",
		"alternatives": alts,
	})
	resp := resultText(res)
	if !strings.Contains(resp, "decision alternatives were truncated") {
		t.Fatalf("21 alternatives must warn: %q", resp)
	}

	session2 := newTitleCapSession(t)
	res2 := callTool(t, session2, "ghost_decision_record", map[string]any{
		"project_id":   "test-project",
		"title":        "decision with long alternative",
		"decision":     "decision",
		"rationale":    "rationale",
		"alternatives": []string{strings.Repeat("A", 301)},
	})
	resp2 := resultText(res2)
	if !strings.Contains(resp2, "decision alternatives were truncated") {
		t.Fatalf("oversized alternative must warn: %q", resp2)
	}

	session3 := newTitleCapSession(t)
	uncut := make([]string, 20)
	for i := range uncut {
		uncut[i] = "alternative"
	}
	res3 := callTool(t, session3, "ghost_decision_record", map[string]any{
		"project_id":   "test-project",
		"title":        "decision with uncut alternatives",
		"decision":     "decision",
		"rationale":    "rationale",
		"alternatives": uncut,
	})
	resp3 := resultText(res3)
	if strings.Contains(resp3, "truncated") {
		t.Fatalf("uncut alternatives must not warn: %q", resp3)
	}
}
