package mcpserver

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wcatz/ghost/internal/reflection"
)

func seedConsolidatableRowsFast(t *testing.T, db *sql.DB, projectID string, n int) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := 0; i < n; i++ {
		if _, err := tx.Exec(
			`INSERT INTO memories (project_id, category, content, source, importance, created_at, updated_at)
			 VALUES (?, 'fact', ?, 'mcp', 0.6, datetime('now', '-1 hour'), datetime('now', '-1 hour'))`,
			projectID, fmt.Sprintf("relay %05d listens on its own port", i)); err != nil {
			_ = tx.Rollback()
			t.Fatalf("insert row %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func TestGhostHealthReflectOverBound(t *testing.T) {
	srv, store, db, _ := diagnosticServer(t)
	ctx := context.Background()

	if err := store.EnsureProject(ctx, "proj-over", "/tmp/proj-over", "over-project"); err != nil {
		t.Fatalf("EnsureProject over: %v", err)
	}
	if err := store.EnsureProject(ctx, "proj-under", "/tmp/proj-under", "under-project"); err != nil {
		t.Fatalf("EnsureProject under: %v", err)
	}

	seedConsolidatableRowsFast(t, db, "proj-over", reflection.MaxConsolidationInput+1)
	seedConsolidatableRowsFast(t, db, "proj-under", 500)

	session := connectedClient(t, srv)
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "ghost_health"})
	if err != nil {
		t.Fatalf("CallTool ghost_health: %v", err)
	}
	if len(res.Content) == 0 {
		t.Fatalf("empty content")
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("want text content")
	}
	out := text.Text

	if !strings.Contains(out, "above the") {
		t.Fatalf("expected reflect over-bound section, got:\n%s", out)
	}
	if !strings.Contains(out, fmt.Sprintf("%d", reflection.MaxConsolidationInput)) {
		t.Fatalf("expected bound mentioned, got:\n%s", out)
	}
	if !strings.Contains(out, "ghost resolve") || !strings.Contains(out, "ghost prune") || !strings.Contains(out, "ghost reflect") {
		t.Fatalf("expected remediation commands, got:\n%s", out)
	}
	if !strings.Contains(out, "over-project") && !strings.Contains(out, "proj-over") {
		t.Fatalf("expected over-bound project named, got:\n%s", out)
	}
	if !strings.Contains(out, fmt.Sprintf("%d", reflection.MaxConsolidationInput+1)) {
		t.Fatalf("expected over-bound count %d, got:\n%s", reflection.MaxConsolidationInput+1, out)
	}
	// Extract the over-bound block: from its header to the next blank line.
	lines := strings.Split(out, "\n")
	start := -1
	for i, l := range lines {
		if strings.Contains(l, "above the") && strings.Contains(l, "limit for a single consolidation") {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("over-bound header not found in output:\n%s", out)
	}
	var block []string
	for i := start; i < len(lines); i++ {
		if i > start && strings.TrimSpace(lines[i]) == "" {
			break
		}
		block = append(block, lines[i])
	}
	blockText := strings.Join(block, "\n")
	if !strings.Contains(blockText, "over-project") && !strings.Contains(blockText, "proj-over") {
		t.Fatalf("over-bound project not named in block:\n%s", blockText)
	}
	if !strings.Contains(blockText, fmt.Sprintf("%d", reflection.MaxConsolidationInput+1)) {
		t.Fatalf("over-bound count not in block:\n%s", blockText)
	}
	if !strings.Contains(blockText, "consolidatable memories") {
		t.Fatalf("per-project bullet missing in block:\n%s", blockText)
	}
	if strings.Contains(blockText, "under-project") || strings.Contains(blockText, "proj-under") {
		t.Fatalf("under-bound project appears in over-bound block:\n%s", blockText)
	}
}
