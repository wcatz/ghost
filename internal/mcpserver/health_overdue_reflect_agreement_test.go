package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wcatz/ghost/internal/reflection"
)

func TestGhostHealthReflectOverBoundAgreementWithOverdueExclusion(t *testing.T) {
	srv, store, db, _ := diagnosticServer(t)
	ctx := context.Background()

	if err := store.EnsureProject(ctx, "proj-over", "/tmp/proj-over", "over-project"); err != nil {
		t.Fatalf("EnsureProject over: %v", err)
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := 0; i < reflection.MaxConsolidationInput+1; i++ {
		if _, err := tx.Exec(
			`INSERT INTO memories (project_id, category, content, source, importance, created_at, updated_at)
			 VALUES (?, 'fact', ?, 'mcp', 0.6, datetime('now', '-1 hour'), datetime('now', '-1 hour'))`,
			"proj-over", fmt.Sprintf("relay %05d listens on its own port", i)); err != nil {
			_ = tx.Rollback()
			t.Fatalf("insert row %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

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
	if !strings.Contains(out, "over-project") && !strings.Contains(out, "proj-over") {
		t.Fatalf("expected over-bound project named, got:\n%s", out)
	}
	if !strings.Contains(out, "ghost resolve") || !strings.Contains(out, "ghost prune") {
		t.Fatalf("expected remediation commands, got:\n%s", out)
	}
}
