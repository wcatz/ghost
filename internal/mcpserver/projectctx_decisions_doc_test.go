package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ghost_project_context renders no `## Recent Decisions` section; only the
// context resource and the recall_project prompt do, which is what the docs
// describe.
func TestProjectContextToolHasNoRecentDecisionsSection(t *testing.T) {
	_, session := newValiditySession(t)
	ctx := context.Background()

	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_memory_save",
		Arguments: map[string]any{"project_id": "dproj", "content": "a claim about dproj", "category": "preference"},
	}); err != nil {
		t.Fatalf("ghost_memory_save: %v", err)
	}
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "ghost_decision_record",
		Arguments: map[string]any{
			"project_id": "dproj", "title": "use sqlite", "decision": "keep one embedded database", "rationale": "no server to run",
		},
	}); err != nil {
		t.Fatalf("ghost_decision_record: %v", err)
	}

	tool := projectContextToolText(t, session, "dproj")
	if !strings.Contains(tool, "## Memories") {
		t.Fatalf("tool answer holds no memories section:\n%s", tool)
	}
	if strings.Contains(tool, "## Recent Decisions") {
		t.Errorf("ghost_project_context rendered a decisions section:\n%s", tool)
	}

	res, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "ghost://project/dproj/context"})
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if !strings.Contains(res.Contents[0].Text, "## Recent Decisions") {
		t.Errorf("context resource lost its decisions section:\n%s", res.Contents[0].Text)
	}

}
