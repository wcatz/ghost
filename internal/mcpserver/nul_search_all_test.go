package mcpserver

import (
	"strings"
	"testing"
)

func TestSearchAllHandlesNULByte(t *testing.T) {
	_, session := newCapSession(t)

	res := callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": "test-project",
		"content":    "x a b y",
		"category":   "fact",
	})
	if strings.Contains(resultText(res), "error") {
		t.Fatalf("save failed: %s", resultText(res))
	}

	out := resultText(callTool(t, session, "ghost_search_all", map[string]any{
		"query": "a\x00b",
	}))
	t.Logf("search_all out: %s", out)
	if strings.Contains(out, "No matching memories found.") {
		t.Fatalf("search_all claimed no matching memories for a matching query: %s", out)
	}
}
