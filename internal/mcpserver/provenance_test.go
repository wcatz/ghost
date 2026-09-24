package mcpserver

import (
	"context"
	"log/slog"
	"os"
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

// TestSaveUsesClientNameAsAgent pins two things at once: a self-reported
// client name beats process ancestry, and the stored value is the canonical
// source token rather than the binary name.
//
// The token matters as much as the precedence. Claude is "claude-code"
// everywhere it appears as a source — SourceForClientName maps it that way,
// detectSourceFromEnv normalizes CLAUDECODE to it, and
// NewSourceProviderForSource only accepts "claude-code". Recording "claude"
// would put rows in the database that no consumer filtering on agent could
// find. An earlier revision of this code documented exactly that wrong enum.
func TestSaveUsesClientNameAsAgent(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClientNamed(t, srv, "claude-code")

	// Ancestry would report a different harness; the client's own name must
	// win, since the client knows what it is better than we can guess.
	old := detectCallingSource
	detectCallingSource = func() string { return "opencode" }
	t.Cleanup(func() { detectCallingSource = old })

	res := callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": "test-project",
		"content":    "saved from a claude-code session",
		"category":   "fact",
	})
	id, ok := extractID(resultText(res))
	if !ok || id == "" {
		t.Fatalf("save response carries no memory id: %q", resultText(res))
	}
	mems, err := store.GetByIDs(context.Background(), []string{id})
	if err != nil || len(mems) != 1 {
		t.Fatalf("GetByIDs(%q): err=%v n=%d", id, err, len(mems))
	}
	if mems[0].Agent != "claude-code" {
		t.Errorf("Agent = %q, want claude-code — the canonical source token, not the binary name and not the ancestry fallback %q", mems[0].Agent, "opencode")
	}
}

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
