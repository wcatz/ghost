package mcpserver

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

var tokenProjectArg = "https://oauth2:" + tokenMarker + "@github.com/wcatz/ghost.git"
var tokenMarker = "ghp" + "_" + strings.Repeat("a", 30)

func seedProjectWithRemote(t *testing.T, store *memory.Store) {
	t.Helper()
	if err := store.EnsureProjectWithRepo(context.Background(), "abc123", "/tmp/test", "test-project", "https://github.com/wcatz/ghost"); err != nil {
		t.Fatalf("EnsureProjectWithRepo: %v", err)
	}
}

func TestGhostResolveResultNeverEchoesTheTokenShapedProjectArgument(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake binary requires a POSIX shell")
	}
	store := testStore(t)
	seedProjectWithRemote(t, store)
	if _, err := store.Create(context.Background(), "abc123", memory.Memory{Category: "gotcha", Content: "root cause: fixed in v2, no further action needed", Source: "mcp", Importance: 0.5}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	dir := t.TempDir()
	writeFakeClaude(t, filepath.Join(dir, "claude"), resolveAnswer)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	session := connectedClientNamed(t, srv, "claude-code")
	res := callTool(t, session, "ghost_resolve", map[string]any{"project": tokenProjectArg})
	text := resultText(res)
	if res.IsError || strings.Contains(text, tokenProjectArg) || strings.Contains(text, tokenMarker) || !strings.Contains(text, "would resolve") {
		t.Fatalf("unexpected result: err=%v\n%s", res.IsError, text)
	}
}

func TestGhostResolveMarkResultNeverEchoesTheTokenShapedProjectArgument(t *testing.T) {
	store := testStore(t)
	seedProjectWithRemote(t, store)
	id, err := store.Create(context.Background(), "abc123", memory.Memory{Category: "fact", Content: "a note the agent has read alongside a newer one", Source: "mcp", Importance: 0.7})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	srv := New(store, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), "test")
	session := connectedClient(t, srv)
	res := callTool(t, session, "ghost_resolve_mark", map[string]any{"project_id": tokenProjectArg, "memory_ids": []string{id}})
	text := resultText(res)
	if res.IsError || strings.Contains(text, tokenProjectArg) || strings.Contains(text, tokenMarker) {
		t.Fatalf("unexpected result: err=%v\n%s", res.IsError, text)
	}
	if !strings.Contains(text, "Marked 1") || !isMarkedResolved(t, store, id) {
		t.Fatalf("mark did not land:\n%s", text)
	}
	if !strings.Contains(text, "ghost resolve abc123 --reassess --only") {
		t.Fatalf("repair command not built from resolved project:\n%s", text)
	}
}

func TestGhostLinkWithdrawResultNeverEchoesTheTokenShapedProjectArgument(t *testing.T) {
	srv, store := linkWithdrawServer(t)
	seedProjectWithRemote(t, store)
	newer, older := seedLinkWithdrawal(t, store, "abc123")
	session := connectedClient(t, srv)
	res := callTool(t, session, "ghost_link_withdraw", map[string]any{"project_id": tokenProjectArg, "source_id": newer, "target_id": older})
	text := resultText(res)
	if res.IsError || strings.Contains(text, tokenProjectArg) || strings.Contains(text, tokenMarker) {
		t.Fatalf("unexpected result: err=%v\n%s", res.IsError, text)
	}
	if !strings.Contains(text, "Withdrew 1 of 1") || liveInto(t, store, "abc123", older) != 0 {
		t.Fatalf("withdraw did not land:\n%s", text)
	}
	if !strings.Contains(text, "ghost resolve abc123 --reassess --only") {
		t.Fatalf("repair command not built from resolved project:\n%s", text)
	}
}
