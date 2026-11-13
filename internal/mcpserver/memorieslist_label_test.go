package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestFilteredMemoriesListLabelsItsBuckets is issue #1755: a filtered browse
// widens to the project plus `_global`, so each row must name its bucket.
func TestFilteredMemoriesListLabelsItsBuckets(t *testing.T) {
	_, session := newValiditySession(t)
	ctx := context.Background()

	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "ghost_memory_save",
		Arguments: map[string]any{
			"project_id": "p1", "content": "a project gotcha", "category": "gotcha",
		},
	}); err != nil {
		t.Fatalf("ghost_memory_save: %v", err)
	}
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_save_global",
		Arguments: map[string]any{"content": "a global gotcha", "category": "gotcha"},
	}); err != nil {
		t.Fatalf("ghost_save_global: %v", err)
	}

	out := resultText(callTool(t, session, "ghost_memories_list", map[string]any{
		"project_id": "p1", "category": "gotcha",
	}))
	if !strings.Contains(out, "project=_global") {
		t.Errorf("filtered list carries no project=_global label:\n%s", out)
	}
	if !strings.Contains(out, "project=p1") {
		t.Errorf("filtered list carries no project=p1 label:\n%s", out)
	}
}
