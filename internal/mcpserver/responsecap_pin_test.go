package mcpserver

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func nearCapContent(i int) string {
	return fmt.Sprintf("bulk %02d ", i) + strings.Repeat(fmt.Sprintf("w%02d ", i), 1970)
}

func savePinned(t *testing.T, session *mcp.ClientSession, tool, pinProject, content string) string {
	t.Helper()
	args := map[string]any{"content": content, "category": "preference"}
	if tool == "ghost_memory_save" {
		args["project_id"] = "test-project"
	}
	res := callTool(t, session, tool, args)
	if res.IsError {
		t.Fatalf("%s: %s", tool, resultText(res))
	}
	id, ok := extractID(resultText(res))
	if !ok {
		t.Fatalf("%s response carries no id: %q", tool, resultText(res))
	}
	pin := callTool(t, session, "ghost_memory_pin", map[string]any{"project_id": pinProject, "memory_id": id, "pinned": true})
	if pin.IsError {
		t.Fatalf("pin %s: %s", id, resultText(pin))
	}
	return id
}

func projectContextOut(t *testing.T, session *mcp.ClientSession) string {
	t.Helper()
	res := callTool(t, session, "ghost_project_context", map[string]any{"project_id": "test-project", "limit": 100})
	if res.IsError {
		t.Fatalf("project_context failed: %s", resultText(res))
	}
	return resultText(res)
}

var emptyHeading = regexp.MustCompile(`(?m)^## [^\n]*\n\n(?:\n|## |\(|$)`)

func assertNoEmptyHeading(t *testing.T, out string) {
	t.Helper()
	if loc := emptyHeading.FindStringIndex(out); loc != nil {
		t.Errorf("empty heading: %q", out[loc[0]:min(loc[1]+40, len(out))])
	}
}

func TestProjectContextByteCutPrefersUnpinnedRows(t *testing.T) {
	_, session := newCapSession(t)
	const n = 6
	for i := range n {
		callTool(t, session, "ghost_memory_save", map[string]any{"project_id": "test-project", "content": nearCapContent(i), "category": "fact"})
	}
	pref := "always sign commits and never force-push " + strings.Repeat("p", 120)
	savePinned(t, session, "ghost_save_global", "_global", pref)

	out := projectContextOut(t, session)
	assertWithinBudget(t, out)
	assertNoEmptyHeading(t, out)
	if !strings.Contains(out, pref) {
		t.Fatalf("pinned global row cut: len=%d %q", len(out), out[max(0, len(out)-300):])
	}
	if cutCount(t, out) < 1 || strings.Contains(out, "cut from the end of this answer") {
		t.Fatalf("unexpected cut notice: %q", out[max(0, len(out)-300):])
	}
}

func TestProjectContextByteCutKeepsPinnedProjectAndReportsPinnedCuts(t *testing.T) {
	_, session := newCapSession(t)
	for i := range 6 {
		callTool(t, session, "ghost_memory_save", map[string]any{"project_id": "test-project", "content": nearCapContent(i), "category": "fact"})
	}
	pinned := "the deploy runbook lives under docs/ops " + strings.Repeat("q", 120)
	savePinned(t, session, "ghost_memory_save", "test-project", pinned)
	out := projectContextOut(t, session)
	assertWithinBudget(t, out)
	assertNoEmptyHeading(t, out)
	if !strings.Contains(out, pinned) || cutCount(t, out) < 1 {
		t.Fatalf("pinned project row missing or no cut: %q", out[max(0, len(out)-300):])
	}

	_, session = newCapSession(t)
	callTool(t, session, "ghost_memory_save", map[string]any{"project_id": "test-project", "content": "small unpinned fact", "category": "fact"})
	for i := range 4 {
		savePinned(t, session, "ghost_memory_save", "test-project", nearCapContent(i))
	}
	out = projectContextOut(t, session)
	assertWithinBudget(t, out)
	assertNoEmptyHeading(t, out)
	if strings.Contains(out, "small unpinned fact") {
		t.Fatalf("unpinned row survived while pinned rows cut: %q", out[max(0, len(out)-300):])
	}
	if cutCount(t, out) < 1 {
		t.Fatalf("expected cut: %q", out[max(0, len(out)-300):])
	}
}
