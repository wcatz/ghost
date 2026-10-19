package mcpserver

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wcatz/ghost/internal/memory"
)

// tokenProjectArg is a clone URL with embedded auth — the ordinary agent
// mistake: it RESOLVES to a project through its normalized remote (the
// userinfo is stripped by NormalizeRepoRemote), so a tool call carrying it
// succeeds, while the value itself is token-shaped and must never come back
// in the result. The token marker is asserted on its own as well as the whole
// URL, so a result that withholds the URL but prints the token still fails.
const tokenProjectArg = "https://oauth2:ghp_0123456789abcdefghijklmnopqrst@github.com/wcatz/ghost.git"

// seedProjectWithRemote gives the testStore project a repository remote, so a
// clone URL of that repository resolves to it the way docs/mcp.md describes.
func seedProjectWithRemote(t *testing.T, store *memory.Store) {
	t.Helper()
	ctx := context.Background()
	if err := store.EnsureProjectWithRepo(ctx, "abc123", "/tmp/test", "test-project", "https://github.com/wcatz/ghost"); err != nil {
		t.Fatalf("EnsureProjectWithRepo: %v", err)
	}
}

// TestGhostResolveResultNeverEchoesTheTokenShapedProjectArgument pins the
// summary line of ghost_resolve: the project is named through the shared
// renderer, never as the caller typed it, so a token-shaped clone URL that
// resolves to the project is not printed back into the agent's context.
func TestGhostResolveResultNeverEchoesTheTokenShapedProjectArgument(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake binary requires a POSIX shell")
	}
	store := testStore(t)
	seedProjectWithRemote(t, store)
	ctx := context.Background()
	// A confirmed-evidence note, so the pass has a row to report on.
	if _, err := store.Create(ctx, "abc123", memory.Memory{
		Category: "gotcha", Content: "root cause: fixed in v2, no further action needed",
		Source: "mcp", Importance: 0.5,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	// A fake `claude` on PATH answers RESOLVED, and the client names itself
	// claude-code so session routing selects the fake rather than probing the
	// ambient harness.
	dir := t.TempDir()
	writeFakeClaude(t, filepath.Join(dir, "claude"), resolveAnswer)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	session := connectedClientNamed(t, srv, "claude-code")

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_resolve",
		Arguments: map[string]any{"project": tokenProjectArg},
	})
	if err != nil {
		t.Fatalf("CallTool ghost_resolve: %v", err)
	}
	if result.IsError {
		t.Fatalf("ghost_resolve returned an error result: %+v", result.Content)
	}
	text := resultText(result)
	if strings.Contains(text, tokenProjectArg) {
		t.Errorf("the result echoes the caller's token-shaped project argument:\n%s", text)
	}
	if strings.Contains(text, "ghp_") {
		t.Errorf("the result prints the token marker from the project argument:\n%s", text)
	}
	// The pass itself ran and reported its verdict, so the silence above is a
	// rendering and not a call that never reached the store.
	if !strings.Contains(text, "would resolve") {
		t.Errorf("the pass did not report its verdict:\n%s", text)
	}
}

// TestGhostResolveMarkResultNeverEchoesTheTokenShapedProjectArgument pins the
// other half: ghost_resolve_mark resolves the project and then builds its
// repair command from the RESOLVED project, so the command an agent can paste
// carries a store id and never the caller's token-shaped spelling.
func TestGhostResolveMarkResultNeverEchoesTheTokenShapedProjectArgument(t *testing.T) {
	store := testStore(t)
	seedProjectWithRemote(t, store)
	ctx := context.Background()
	id, err := store.Create(ctx, "abc123", memory.Memory{
		Category: "fact", Content: "a note the agent has read alongside a newer one",
		Source: "mcp", Importance: 0.7,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_resolve_mark",
		Arguments: map[string]any{"project_id": tokenProjectArg, "memory_ids": []string{id}},
	})
	if err != nil {
		t.Fatalf("CallTool ghost_resolve_mark: %v", err)
	}
	if result.IsError {
		t.Fatalf("ghost_resolve_mark returned an error result: %+v", result.Content)
	}
	text := resultText(result)
	if strings.Contains(text, tokenProjectArg) {
		t.Errorf("the result echoes the caller's token-shaped project argument:\n%s", text)
	}
	if strings.Contains(text, "ghp_") {
		t.Errorf("the result prints the token marker from the project argument:\n%s", text)
	}
	// The mark landed, so the silence above is a rendering and not a call that
	// never reached the store.
	if !strings.Contains(text, "Marked 1") {
		t.Errorf("the tool did not report its stamp:\n%s", text)
	}
	if !isMarkedResolved(t, store, id) {
		t.Error("the named memory is not resolved")
	}
	// The repair command is scoped to the project the argument RESOLVED to.
	if !strings.Contains(text, "ghost resolve abc123 --reassess --only") {
		t.Errorf("the repair command is not built from the resolved project:\n%s", text)
	}
}
