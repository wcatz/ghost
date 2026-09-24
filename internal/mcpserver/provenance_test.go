package mcpserver

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// saveAndReadAgent saves through the live ghost_memory_save tool and returns
// the agent provenance that actually reached the store for that memory.
func saveAndReadAgent(t *testing.T, srv *Server, session *mcp.ClientSession, content string) string {
	t.Helper()
	res := callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": "test-project",
		"content":    content,
		"category":   "fact",
	})
	id, ok := extractID(resultText(res))
	if !ok || id == "" {
		t.Fatalf("save response carries no memory id: %q", resultText(res))
	}
	mems, err := srv.store.GetByIDs(context.Background(), []string{id})
	if err != nil || len(mems) != 1 {
		t.Fatalf("GetByIDs(%q): err=%v n=%d", id, err, len(mems))
	}
	return mems[0].Agent
}

// TestSaveRecordsAgentProvenance: the store accepting provenance is not the
// feature — no production caller passing it means every memory saved through
// MCP keeps NULL, and the columns stay unreachable from the product's normal
// write path. This drives the actual tool handler.
//
// detectCallingSource is pinned rather than read from the environment: the
// test process's own ancestor chain can legitimately contain a harness
// (running `go test` from an opencode or claude session), which would make
// this test assert whatever happens to be running it.
func TestSaveRecordsAgentProvenance(t *testing.T) {
	srv, session := newCapSession(t)

	old := detectCallingSource
	detectCallingSource = func() string { return "opencode" }
	t.Cleanup(func() { detectCallingSource = old })

	agent := saveAndReadAgent(t, srv, session, "the gateway listens on port 8443")
	if agent != "opencode" {
		t.Errorf("Agent = %q, want opencode — ghost_memory_save must pass provenance through to the store", agent)
	}
}

// TestSaveWithoutDetectableAgentStaysUnknown: when no harness is detectable,
// the save must still succeed and must record nothing rather than guessing.
// A fabricated agent is worse than none — it is exactly the provenance these
// columns exist to make trustworthy.
func TestSaveWithoutDetectableAgentStaysUnknown(t *testing.T) {
	srv, session := newCapSession(t)

	old := detectCallingSource
	detectCallingSource = func() string { return "" }
	t.Cleanup(func() { detectCallingSource = old })

	agent := saveAndReadAgent(t, srv, session, "a memory whose author is genuinely unknown")
	if agent != "" {
		t.Errorf("Agent = %q, want empty — an undetectable harness must not be recorded as an author", agent)
	}
}
