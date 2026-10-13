package mcpserver

// ghost_resolve's applied listing is Run's second return rendered by the
// handler's line loop (mcpserver.go), and since #1052's freshness write a
// confirmed row can be REFUSED: rewritten while it was being classified, it
// stays live with its new text and is counted as stale. The listing must then
// show it neither as resolved nor with the pre-write text a verdict was reached
// on. Run filters its applied return to the stamped rows; this test proves the
// refusal reaches the ghost_resolve OUTPUT end to end — through a real
// classifier subprocess, not a fake store.

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

// TestGhostResolve_AppliedListingOmitsStaleRow drives the ghost_resolve handler
// with a fake claude that answers RESOLVED, while a watcher goroutine rewrites
// one of the two confirmed notes INSIDE the read-to-write window. The ordering
// is a two-file handshake so the rewrite provably lands between the pass's read
// and its write rather than around a sleep: the fake binary touches a sentinel
// once it runs (classification has started, so the candidates are already
// read), the watcher then rewrites the note and creates the proceed file, and
// the fake binary answers only once proceed exists — after which the write in
// the handler must refuse the rewritten row.
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
	// The harness env is allow-listed (internal/ai/harness_env.go); the two
	// handshake paths are opt-in via GHOST_PASSTHROUGH_ENV.
	t.Setenv("GHOST_PASSTHROUGH_ENV", "GHOST_RESOLVE_SENTINEL,GHOST_RESOLVE_PROCEED")
	t.Setenv("GHOST_RESOLVE_SENTINEL", sentinel)
	t.Setenv("GHOST_RESOLVE_PROCEED", proceed)

	session := connectedClientNamed(t, srv, "claude-code")

	rewriteDone := make(chan error, 1)
	go func() {
		// Wait for classification to start (the fake binary ran), then rewrite
		// the stale row and release the classifier. A bound exists so a broken
		// handshake fails the test instead of hanging it.
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

	// The listing names only the stamped row: the rewritten row's short id and
	// judged text are gone, the summary names it stale, and the untouched row is
	// listed under the text the store holds.
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

	// The store agrees with the listing: the untouched row is stamped under its
	// judged text, the rewritten row keeps the NEW text and stays live.
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
