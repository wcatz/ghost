package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/reflection"
)

const healthTokenFixture = "ghp_0123456789abcdefghijklmnopqrstuvwxyzAB"

func TestGhostHealthWithholdsTokenShapedProjectValues(t *testing.T) {
	srv, store, db, _ := diagnosticServer(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, healthTokenFixture, "/tmp/tok-health", "tok-health-name"); err != nil {
		t.Fatalf("EnsureProject token id: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO projects (id, path, name, repo_remote) VALUES (?, ?, ?, ?)`, "nameproj-health", "/tmp/nameproj-health", healthTokenFixture, ""); err != nil {
		t.Fatalf("plant token-name project: %v", err)
	}
	if _, err := store.Create(ctx, healthTokenFixture, memory.Memory{Category: "fact", Content: "health row", Importance: 0.5, Source: "mcp"}); err != nil {
		t.Fatalf("Create token id: %v", err)
	}
	if _, err := store.Create(ctx, "nameproj-health", memory.Memory{Category: "fact", Content: "health row", Importance: 0.5, Source: "mcp"}); err != nil {
		t.Fatalf("Create token name: %v", err)
	}
	session := connectedClient(t, srv)
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "ghost_health"})
	if err != nil {
		t.Fatalf("CallTool ghost_health: %v", err)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("ghost_health returned %T, want text", res.Content[0])
	}
	out := text.Text
	if strings.Contains(out, healthTokenFixture) {
		t.Errorf("ghost_health printed a token-shaped project value whole:\n%s", out)
	}
	if strings.Contains(out, "ghp_0123") {
		t.Errorf("ghost_health printed a truncated token-shaped project id:\n%s", out)
	}
	if !strings.Contains(out, "withheld") {
		t.Errorf("ghost_health did not withhold the token-shaped value:\n%s", out)
	}
	if !strings.Contains(out, "tok-health-name") || !strings.Contains(out, "test-project") {
		t.Errorf("ghost_health changed an ordinary project:\n%s", out)
	}
}

func TestGhostHealthReflectOverBoundWithholdsTokenShapedProjectID(t *testing.T) {
	srv, store, db, _ := diagnosticServer(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, healthTokenFixture, "/tmp/tok-over-health", "tok2name"); err != nil {
		t.Fatalf("EnsureProject token id: %v", err)
	}
	seedConsolidatableRowsFast(t, db, healthTokenFixture, reflection.MaxConsolidationInput+1)
	_ = fmt.Sprint(ctx)
	session := connectedClient(t, srv)
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "ghost_health"})
	if err != nil {
		t.Fatalf("CallTool ghost_health: %v", err)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("ghost_health returned %T, want text", res.Content[0])
	}
	out := text.Text
	if !strings.Contains(out, "above the") {
		t.Fatalf("expected reflect over-bound section, got:\n%s", out)
	}
	if strings.Contains(out, healthTokenFixture) {
		t.Errorf("ghost_health reflect over-bound printed a token-shaped project id whole:\n%s", out)
	}
	if strings.Contains(out, "ghp_0123") {
		t.Errorf("ghost_health reflect over-bound printed a truncated token-shaped project id:\n%s", out)
	}
	if !strings.Contains(out, "withheld") {
		t.Errorf("ghost_health reflect over-bound did not withhold the token-shaped id:\n%s", out)
	}
	if !strings.Contains(out, "tok2name") {
		t.Errorf("ghost_health reflect over-bound lost an ordinary name:\n%s", out)
	}
}
