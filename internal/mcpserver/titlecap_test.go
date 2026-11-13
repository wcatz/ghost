package mcpserver

import (
	"log/slog"
	"os"
	"strings"
	"testing"
)

func TestTitleAndAlternativeTruncationWarnings(t *testing.T) {
	session := connectedClient(t, New(testStore(t), slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), "test"))
	for _, tc := range []struct {
		tool string
		args map[string]any
		want string
		none bool
	}{
		{"ghost_task_create", map[string]any{"project_id": "test-project", "title": strings.Repeat("T", 301)}, "task title was truncated at 300 bytes", false},
		{"ghost_task_create", map[string]any{"project_id": "test-project", "title": strings.Repeat("T", 300)}, "truncated", true},
		{"ghost_decision_record", map[string]any{"project_id": "test-project", "title": strings.Repeat("T", 301), "decision": "d", "rationale": "r"}, "decision title was truncated at 300 bytes", false},
		{"ghost_decision_record", map[string]any{"project_id": "test-project", "title": strings.Repeat("T", 300), "decision": "d", "rationale": "r"}, "decision title was truncated", true},
	} {
		resp := resultText(callTool(t, session, tc.tool, tc.args))
		if tc.none {
			if strings.Contains(resp, tc.want) {
				t.Fatalf("%s %v: %q", tc.tool, tc.args["title"], resp)
			}
		} else if !strings.Contains(resp, tc.want) {
			t.Fatalf("%s %v: %q", tc.tool, tc.args["title"], resp)
		}
	}
	alts := make([]string, 21)
	for i := range alts {
		alts[i] = "alternative"
	}
	resp := resultText(callTool(t, session, "ghost_decision_record", map[string]any{
		"project_id": "test-project", "title": "t", "decision": "d", "rationale": "r", "alternatives": alts,
	}))
	if !strings.Contains(resp, "decision alternatives were truncated") {
		t.Fatalf("too many alternatives: %q", resp)
	}
	resp = resultText(callTool(t, session, "ghost_decision_record", map[string]any{
		"project_id": "test-project", "title": "t2", "decision": "d", "rationale": "r", "alternatives": []string{strings.Repeat("A", 301)},
	}))
	if !strings.Contains(resp, "decision alternatives were truncated") {
		t.Fatalf("oversized alternative: %q", resp)
	}
	uncut := make([]string, 20)
	for i := range uncut {
		uncut[i] = "alternative"
	}
	resp = resultText(callTool(t, session, "ghost_decision_record", map[string]any{
		"project_id": "test-project", "title": "t3", "decision": "d", "rationale": "r", "alternatives": uncut,
	}))
	if strings.Contains(resp, "truncated") {
		t.Fatalf("uncut alternatives warned: %q", resp)
	}
}
