package main

// The resolved LISTING a `ghost resolve --apply` prints is Run's second return
// rendered by memoryLines (lifecycle.go), and since #1052's freshness write a
// confirmed row can be REFUSED: rewritten (or promoted) while it was being
// classified, it stays live with its new text and is counted as stale. The
// listing must then show it neither as resolved nor with the pre-write text a
// verdict was reached on — the row the store holds may not be the row the
// classifier judged. Run filters its applied return to the stamped rows, and
// this test renders the CLI listing to prove the stale row disappears from it.

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/resolve"
)

// resolveRewriteClassifier answers RESOLVED for every note and rewrites one note
// before returning, standing in for the concurrent save that lands mid-classify
// — the race #1052 reproduces.
type resolveRewriteClassifier struct {
	store   *memory.Store
	project string
	id      string
	newText string
}

func (c *resolveRewriteClassifier) IsResolvedBatch(ctx context.Context, contents []string) ([]resolve.Verdict, error) {
	if err := c.store.UpdateMemory(ctx, c.project, c.id, &c.newText, nil, nil, nil); err != nil {
		return nil, err
	}
	out := make([]resolve.Verdict, len(contents))
	for i := range out {
		out[i] = resolve.VerdictResolved
	}
	return out, nil
}

// TestResolveListingOmitsStaleRow drives the CLI's own renderer over Run's
// applied return: one confirmed note is rewritten mid-pass, and the listing
// must contain only the untouched stamped note — not the stale note's id, and
// not its judged (now-gone) text.
func TestResolveListingOmitsStaleRow(t *testing.T) {
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := memory.NewStore(db, logger)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "proj", "/tmp/proj", "test-project"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	staleText := "the migration was shipped in v2 and is closed"
	freshText := "the graph experiment was reverted after the postmortem"
	staleID, err := s.Create(ctx, "proj", memory.Memory{Category: "fact", Content: staleText, Importance: 0.5, Source: "mcp"})
	if err != nil {
		t.Fatalf("Create stale: %v", err)
	}
	freshID, err := s.Create(ctx, "proj", memory.Memory{Category: "fact", Content: freshText, Importance: 0.5, Source: "mcp"})
	if err != nil {
		t.Fatalf("Create fresh: %v", err)
	}
	newText := "the migration rollback runbook is still the live procedure; keep following it"
	cls := &resolveRewriteClassifier{store: s, project: "proj", id: staleID, newText: newText}

	res, applied, err := resolve.Run(ctx, s, cls, "proj", true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Resolved != 1 || res.Stale != 1 {
		t.Fatalf("Resolved = %d, Stale = %d; want 1 and 1", res.Resolved, res.Stale)
	}

	out := memoryLines(applied)
	if strings.Contains(out, shortID(staleID)) {
		t.Errorf("CLI resolve listing still names the stale row %s:\n%s", shortID(staleID), out)
	}
	if strings.Contains(out, staleText) {
		t.Errorf("CLI resolve listing still shows the stale row's judged text (%q), which the store no longer holds:\n%s", staleText, out)
	}
	if !strings.Contains(out, shortID(freshID)) || !strings.Contains(out, freshText) {
		t.Errorf("CLI resolve listing is missing the stamped row %s:\n%s", shortID(freshID), out)
	}
}
