package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestProjectContextToolHasNoRecentDecisionsSection(t *testing.T) {
	_, session := newValiditySession(t)
	ctx := context.Background()
	callTool(t, session, "ghost_memory_save", map[string]any{"project_id": "dproj", "content": "a claim about dproj", "category": "preference"})
	callTool(t, session, "ghost_decision_record", map[string]any{"project_id": "dproj", "title": "use sqlite", "decision": "keep one embedded database", "rationale": "no server to run"})

	tool := projectContextToolText(t, session, "dproj")
	if !strings.Contains(tool, "## Memories") || strings.Contains(tool, "## Recent Decisions") {
		t.Fatalf("tool decisions section mismatch:\n%s", tool)
	}
	res, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "ghost://project/dproj/context"})
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if !strings.Contains(res.Contents[0].Text, "## Recent Decisions") {
		t.Errorf("context resource lost its decisions section:\n%s", res.Contents[0].Text)
	}
}
