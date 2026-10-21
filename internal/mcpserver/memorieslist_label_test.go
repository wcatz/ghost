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

// The label must follow the filter predicate ListMemories uses, so a retention
// filter alone widens the same way a category filter does.
func TestFilteredMemoriesListLabelsItsBucketsOnRetention(t *testing.T) {
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
		"project_id": "p1", "retention": "project",
	}))
	if !strings.Contains(out, "project=_global") {
		t.Errorf("retention-filtered list carries no project=_global label:\n%s", out)
	}
	if !strings.Contains(out, "project=p1") {
		t.Errorf("retention-filtered list carries no project=p1 label:\n%s", out)
	}
}

// An unfiltered browse is the project's own rows alone and carries no bucket.
func TestUnfilteredMemoriesListCarriesNoBucketLabel(t *testing.T) {
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

	out := resultText(callTool(t, session, "ghost_memories_list", map[string]any{
		"project_id": "p1",
	}))
	if strings.Contains(out, "project=") {
		t.Errorf("unfiltered list carries a bucket label:\n%s", out)
	}
}

// A project id that needs quoting renders through the same label renderer as
// ghost_search_all.
func TestFilteredMemoriesListQuotesProjectIDInBucketLabel(t *testing.T) {
	_, session := newValiditySession(t)
	ctx := context.Background()

	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "ghost_memory_save",
		Arguments: map[string]any{
			"project_id": "my proj", "content": "a project gotcha", "category": "gotcha",
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
		"project_id": "my proj", "category": "gotcha",
	}))
	if !strings.Contains(out, `project="my proj"`) {
		t.Errorf("filtered list does not quote the project id like ghost_search_all:\n%s", out)
	}
	if !strings.Contains(out, "project=_global") {
		t.Errorf("filtered list carries no project=_global label:\n%s", out)
	}
}
