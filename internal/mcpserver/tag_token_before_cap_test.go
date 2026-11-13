package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/memory"
)

func tokenPastTagCap() (tag, token string) {
	token = "ghp_" + strings.Repeat("b", 36)
	return strings.Repeat("a", tagMaxLen) + " " + token, token
}

func tagWriters() []struct {
	name    string
	project string
	call    func(t *testing.T, srv *Server, s *mcp.ClientSession, tags []string) *mcp.CallToolResult
	seeded  int
} {
	return []struct {
		name    string
		project string
		call    func(t *testing.T, srv *Server, s *mcp.ClientSession, tags []string) *mcp.CallToolResult
		seeded  int
	}{
		{name: "ghost_memory_save", project: "abc123", call: func(t *testing.T, _ *Server, s *mcp.ClientSession, tags []string) *mcp.CallToolResult {
			return callTool(t, s, "ghost_memory_save", map[string]any{"project_id": "test-project", "content": "a claim with a long tag", "category": "fact", "tags": tags})
		}},
		{name: "ghost_save_global", project: memory.GlobalProjectID, call: func(t *testing.T, _ *Server, s *mcp.ClientSession, tags []string) *mcp.CallToolResult {
			return callTool(t, s, "ghost_save_global", map[string]any{"content": "a cross-project preference with a long tag", "category": "preference", "tags": tags})
		}},
		{name: "ghost_memory_update", project: "abc123", seeded: 1, call: func(t *testing.T, srv *Server, s *mcp.ClientSession, tags []string) *mcp.CallToolResult {
			id, err := srv.store.Create(context.Background(), "abc123", memory.Memory{Category: "fact", Content: "a claim to be re-tagged", Source: "mcp", Importance: 0.7, Tags: []string{}})
			if err != nil {
				t.Fatal(err)
			}
			return callTool(t, s, "ghost_memory_update", map[string]any{"project_id": "test-project", "memory_id": id, "tags": tags})
		}},
		{name: "ghost_decision_record", project: "abc123", call: func(t *testing.T, _ *Server, s *mcp.ClientSession, tags []string) *mcp.CallToolResult {
			return callTool(t, s, "ghost_decision_record", map[string]any{"project_id": "test-project", "title": "a decision with a long tag", "decision": "cut long tags", "rationale": "a tag is a short label", "tags": tags})
		}},
	}
}

func TestEveryTagWriterChecksTheUncutTagForATokenBeforeCuttingIt(t *testing.T) {
	for _, w := range tagWriters() {
		t.Run(w.name, func(t *testing.T) {
			srv, session := newCapSession(t)
			tag, token := tokenPastTagCap()
			res := w.call(t, srv, session, []string{"ops", tag})
			text := resultText(res)
			if !res.IsError || !strings.Contains(text, "in tags[1]") || strings.Contains(text, token) {
				t.Fatalf("%q", text)
			}
			if got := countProjectMemories(t, srv, w.project); got != w.seeded {
				t.Fatalf("rows=%d want %d", got, w.seeded)
			}
		})
	}
}

func TestEveryTagWriterStillCutsALongTagWithNoToken(t *testing.T) {
	for _, w := range tagWriters() {
		t.Run(w.name, func(t *testing.T) {
			srv, session := newCapSession(t)
			res := w.call(t, srv, session, []string{strings.Repeat("c", 100)})
			if res.IsError {
				t.Fatalf("%q", resultText(res))
			}
			got := storedTags(t, srv, w.project)
			if len(got) != 1 || got[0] != strings.Repeat("c", tagMaxLen) {
				t.Fatalf("stored tags %v", got)
			}
		})
	}
}
