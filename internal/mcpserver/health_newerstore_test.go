package mcpserver

// #746, the reporting half: an agent that cannot write has to be able to ASK
// why, and the answer has to name the two versions and the restart.
//
// ghost_health is the tool an agent or a person reaches for when memory
// features look inactive, and "saves are being refused" is exactly the state
// that looks like a broken install. Without this the refusal only appears as a
// failed tool call, and the agent has no way to learn that the fix is a client
// restart rather than a retry.
//
// The store is FILE-backed and the server is built BEFORE the stamp moves,
// because that is the whole incident: a running server holds a handle it opened
// hours earlier, and a newer binary migrates the file underneath it. A server
// opened afterwards never gets the chance to be wrong — OpenDB refuses a newer
// store outright, which is the check that works and the one that was always in
// place.

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/memory"
)

// healthFixture is a running server and the file behind it, in the order the
// incident happened: the handle is opened first, the file is migrated after.
type healthFixture struct {
	srv  *Server
	path string
}

func newHealthFixture(t *testing.T) *healthFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ghost.db")
	db, err := memory.OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store := memory.NewStore(db, logger)
	if err := store.EnsureProject(context.Background(), "abc123", "/tmp/test", "test-project"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	return &healthFixture{srv: New(store, logger, "test"), path: path}
}

// stampNewer is a newer ghost migrating the file underneath the running server.
func (f *healthFixture) stampNewer(t *testing.T) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+f.path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open second handle: %v", err)
	}
	defer db.Close() //nolint:errcheck
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", memory.SchemaVersion()+1)); err != nil {
		t.Fatalf("stamp store newer: %v", err)
	}
}

func (f *healthFixture) health(t *testing.T) string {
	t.Helper()
	result, err := connectedClient(t, f.srv).CallTool(context.Background(), &mcp.CallToolParams{Name: "ghost_health"})
	if err != nil {
		t.Fatalf("CallTool ghost_health: %v", err)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("ghost_health returned %T, want text", result.Content[0])
	}
	return text.Text
}

func TestHealthReportsAStoreNewerThanThisBuild(t *testing.T) {
	f := newHealthFixture(t)

	// The control: a healthy server must not claim to be in the state this
	// reports, or the warning is noise an operator learns to skip.
	healthy := f.health(t)
	if strings.Contains(healthy, "restart the client") {
		t.Errorf("health on a current store already warns about a restart:\n%s", healthy)
	}

	f.stampNewer(t)

	// The SAME running server, asked again. This is the call the issue is about:
	// the agent notices its saves failing and asks the diagnostic tool why.
	report := f.health(t)
	if !strings.Contains(report, "restart the client that runs this ghost server") {
		t.Errorf("health does not tell the operator to restart the client:\n%s", report)
	}
	// Both versions, so the report says which side moved.
	if !strings.Contains(report, fmt.Sprintf("v%d", memory.SchemaVersion()+1)) {
		t.Errorf("health does not name the store's version v%d:\n%s", memory.SchemaVersion()+1, report)
	}
	if !strings.Contains(report, fmt.Sprintf("v%d", memory.SchemaVersion())) {
		t.Errorf("health does not name this build's version v%d:\n%s", memory.SchemaVersion(), report)
	}
	// The state must read as a REFUSAL, not as a low count: an agent reading
	// "0 memories" would go looking for lost data instead of a stale server.
	if !strings.Contains(report, "refus") {
		t.Errorf("health does not say writes are being refused:\n%s", report)
	}
	// Reads still work, and health is a read, so it must still run at all.
	if !strings.Contains(report, "Ghost Health") {
		t.Errorf("health stopped reporting its own sections:\n%s", report)
	}
}

// A write tool must say the same thing the health report does, in the place the
// agent is actually looking when the save fails. An error that names the store
// version but not the remedy leaves the agent retrying a write that can never
// succeed.
//
// It goes through the TOOL, not the store, because the store is not where the
// text can be lost: a handler that wraps or replaces the error is an ordinary
// thing to write, and only the tool call proves the sentence arrives.
func TestWriteToolErrorNamesTheRestartNotJustTheVersions(t *testing.T) {
	f := newHealthFixture(t)
	ctx := context.Background()
	if _, err := f.srv.store.Create(ctx, "abc123", memory.Memory{
		Category: "fact", Content: "written before the migration", Source: "mcp", Importance: 0.5,
	}); err != nil {
		t.Fatalf("Create before the stamp: %v", err)
	}

	// The control: the same call succeeds while the store is this build's, so
	// the refusal below is about the stamp and not about the arguments.
	saved, err := connectedClient(t, f.srv).CallTool(ctx, &mcp.CallToolParams{
		Name: "ghost_memory_save",
		Arguments: map[string]any{
			"project_id": "test-project",
			"content":    "a save that lands",
		},
	})
	if err != nil {
		t.Fatalf("ghost_memory_save before the stamp: %v", err)
	}
	if saved.IsError {
		t.Fatalf("ghost_memory_save failed on a current store: %+v", saved.Content)
	}

	f.stampNewer(t)

	result, err := connectedClient(t, f.srv).CallTool(ctx, &mcp.CallToolParams{
		Name: "ghost_memory_save",
		Arguments: map[string]any{
			"project_id": "test-project",
			"content":    "a save that must be refused",
		},
	})
	if err != nil {
		t.Fatalf("CallTool ghost_memory_save after the stamp: %v", err)
	}
	if !result.IsError {
		t.Fatal("ghost_memory_save succeeded against a store stamped newer than this build — the agent would keep saving into it")
	}
	msg := toolResultText(t, result)
	if !strings.Contains(msg, "restart the client that runs this ghost server") {
		t.Errorf("write error does not name the restart, so the agent has a failure and no remedy:\n%s", msg)
	}
	if !strings.Contains(msg, fmt.Sprintf("v%d", memory.SchemaVersion()+1)) {
		t.Errorf("write error does not name the store's version:\n%s", msg)
	}
	if !strings.Contains(msg, "reads keep working") {
		t.Errorf("write error reads as a dead server rather than a restart:\n%s", msg)
	}
}

// toolResultText renders a tool result's text content, which is where an error
// returned by a handler arrives.
func toolResultText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	if result == nil || len(result.Content) == 0 {
		t.Fatal("tool result carries no content, so there is no message to read")
	}
	var sb strings.Builder
	for _, c := range result.Content {
		if text, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(text.Text)
		}
	}
	if sb.Len() == 0 {
		t.Fatalf("tool result carried %d content items and none of them was text", len(result.Content))
	}
	return sb.String()
}
