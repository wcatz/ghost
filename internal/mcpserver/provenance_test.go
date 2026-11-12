package mcpserver

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func saveAndReadAgent(t *testing.T, srv *Server, session *mcp.ClientSession, content string) string {
	t.Helper()
	res := callTool(t, session, "ghost_memory_save", map[string]any{"project_id": "test-project", "content": content, "category": "fact"})
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

func TestSaveRecordsAgentProvenance(t *testing.T) {
	srv, session := newCapSession(t)
	old := detectCallingSource
	detectCallingSource = func() string { return "opencode" }
	t.Cleanup(func() { detectCallingSource = old })
	if agent := saveAndReadAgent(t, srv, session, "the gateway listens on port 8443"); agent != "opencode" {
		t.Errorf("Agent = %q, want opencode", agent)
	}
}

func TestSaveUsesClientNameAsAgent(t *testing.T) {
	store := testStore(t)
	srv := New(store, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), "test")
	session := connectedClientNamed(t, srv, "claude-code")
	old := detectCallingSource
	detectCallingSource = func() string { return "opencode" }
	t.Cleanup(func() { detectCallingSource = old })
	res := callTool(t, session, "ghost_memory_save", map[string]any{"project_id": "test-project", "content": "saved from a claude-code session", "category": "fact"})
	id, _ := extractID(resultText(res))
	mems, err := store.GetByIDs(context.Background(), []string{id})
	if err != nil || len(mems) != 1 || mems[0].Agent != "claude-code" {
		t.Fatalf("Agent = %q, want claude-code (err=%v n=%d)", mems[0].Agent, err, len(mems))
	}
}

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
