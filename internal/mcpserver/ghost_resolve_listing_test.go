package mcpserver

// handler's line loop (mcpserver.go), and since #1052's freshness write a

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/memory"
)

func TestGhostResolve_AppliedListingOmitsStaleRow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake binary requires a POSIX shell")
	}
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")

	ctx := context.Background()
	staleText := "root cause: fixed in v2, no further action needed"
	freshText := "graph experiment: reverted after the postmortem, closed"
	staleID, err := store.Create(ctx, "abc123", memory.Memory{Category: "fact", Content: staleText, Importance: 0.5, Source: "mcp"})
	if err != nil {
		t.Fatalf("Create stale: %v", err)
	}
	freshID, err := store.Create(ctx, "abc123", memory.Memory{Category: "fact", Content: freshText, Importance: 0.5, Source: "mcp"})
	if err != nil {
		t.Fatalf("Create fresh: %v", err)
	}

	dir := t.TempDir()
	sentinel := filepath.Join(dir, "classifying")
	proceed := filepath.Join(dir, "proceed")
	script := `#!/bin/sh
if [ "$1" = "--help" ]; then
  printf '%s\n' '--safe-mode' '--restricted' '--strict-mcp-config' '--disable-slash-commands' '--tools' '--disallowedTools' '--setting-sources'
  exit 0
fi
touch "` + sentinel + `"
i=0
while [ ! -f "` + proceed + `" ] && [ $i -lt 400 ]; do sleep 0.05; i=$((i+1)); done
printf '%s' 'RESOLVED | closed-by: the tracking issue closed'
`
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude binary: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GHOST_PASSTHROUGH_ENV", "GHOST_RESOLVE_SENTINEL,GHOST_RESOLVE_PROCEED")
	t.Setenv("GHOST_RESOLVE_SENTINEL", sentinel)
	t.Setenv("GHOST_RESOLVE_PROCEED", proceed)

	session := connectedClientNamed(t, srv, "claude-code")

	rewriteDone := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(sentinel); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		newText := "the migration rollback runbook is still the live procedure; keep following it"
		if err := store.UpdateMemory(ctx, "abc123", staleID, &newText, nil, nil, nil); err != nil {
			rewriteDone <- err
			return
		}
		if err := os.WriteFile(proceed, []byte("go"), 0o644); err != nil {
			rewriteDone <- err
			return
		}
		rewriteDone <- nil
	}()

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_resolve",
		Arguments: map[string]any{"project": "test-project", "apply": true},
	})
	if err != nil {
		t.Fatalf("CallTool ghost_resolve: %v", err)
	}
	if result.IsError {
		t.Fatalf("ghost_resolve returned an error result: %+v", result.Content)
	}
	if err := <-rewriteDone; err != nil {
		t.Fatalf("watcher: %v", err)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Content[0])
	}

	if strings.Contains(text.Text, shortID(staleID)) {
		t.Errorf("ghost_resolve listing still names the stale row %s:\n%s", shortID(staleID), text.Text)
	}
	if strings.Contains(text.Text, staleText) {
		t.Errorf("ghost_resolve listing still shows the stale row's judged text (%q), which the store no longer holds:\n%s", staleText, text.Text)
	}
	if !strings.Contains(text.Text, "1 skipped as stale") {
		t.Errorf("ghost_resolve summary does not report 1 skipped as stale:\n%s", text.Text)
	}
	if !strings.Contains(text.Text, shortID(freshID)) || !strings.Contains(text.Text, freshText) {
		t.Errorf("ghost_resolve listing is missing the stamped row %s:\n%s", shortID(freshID), text.Text)
	}

	rows, err := store.GetByIDs(ctx, []string{staleID, freshID})
	if err != nil || len(rows) != 2 {
		t.Fatalf("GetByIDs: %v (%d rows)", err, len(rows))
	}
	byID := make(map[string]memory.Memory, len(rows))
	for _, r := range rows {
		byID[r.ID] = r
	}
	if got := byID[staleID]; got.ResolvedAt != nil {
		t.Errorf("rewritten row resolved_at = %v, want NULL (stays live)", got.ResolvedAt)
	}
	if got := byID[freshID]; got.ResolvedAt == nil {
		t.Errorf("untouched row resolved_at = NULL, want a stamp")
	}
}
