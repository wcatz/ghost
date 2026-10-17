package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/memory"
)

// validateTags cuts a tag longer than tagMaxLen (64 bytes) before the store's
// token check sees it. The token check therefore has to run on the tag as the
// caller sent it: a token-shaped value placed past byte 64 would otherwise be
// cut out of sight and the call would answer as a success. These tests place
// the token just past the tag bound, through each of the four tools that call
// validateTags, and require a refusal that names the tag with nothing written.
// The control is a 100-byte tag with no token, which is still cut and stored.

// tokenPastTagCap is 64 'a', a space, then a token-shaped value, so the token
// lies entirely past the 64-byte tag bound. Assembled from a repeat so no
// complete provider token appears in the source.
func tokenPastTagCap() (tag, token string) {
	token = "ghp_" + strings.Repeat("b", 36)
	return strings.Repeat("a", tagMaxLen) + " " + token, token
}

// tagWriter calls one tag-bearing tool with the given tags. project is where
// the tool writes, and seeded is how many memory rows call itself writes before
// the tool runs (an update needs a row to update), so a refusal can be checked
// to leave the row count unchanged.
type tagWriter struct {
	name    string
	project string
	call    func(t *testing.T, srv *Server, s *mcp.ClientSession, tags []string) *mcp.CallToolResult
	seeded  int
}

func tagWriters() []tagWriter {
	return []tagWriter{
		{
			name: "ghost_memory_save", project: "abc123",
			call: func(t *testing.T, _ *Server, s *mcp.ClientSession, tags []string) *mcp.CallToolResult {
				return callTool(t, s, "ghost_memory_save", map[string]any{
					"project_id": "test-project", "content": "a claim with a long tag",
					"category": "fact", "tags": tags,
				})
			},
		},
		{
			name: "ghost_save_global", project: memory.GlobalProjectID,
			call: func(t *testing.T, _ *Server, s *mcp.ClientSession, tags []string) *mcp.CallToolResult {
				return callTool(t, s, "ghost_save_global", map[string]any{
					"content":  "a cross-project preference with a long tag",
					"category": "preference", "tags": tags,
				})
			},
		},
		{
			name: "ghost_memory_update", project: "abc123", seeded: 1,
			call: func(t *testing.T, srv *Server, s *mcp.ClientSession, tags []string) *mcp.CallToolResult {
				id, err := srv.store.Create(context.Background(), "abc123", memory.Memory{
					Category: "fact", Content: "a claim to be re-tagged", Source: "mcp",
					Importance: 0.7, Tags: []string{},
				})
				if err != nil {
					t.Fatalf("Create: %v", err)
				}
				return callTool(t, s, "ghost_memory_update", map[string]any{
					"project_id": "test-project", "memory_id": id, "tags": tags,
				})
			},
		},
		{
			name: "ghost_decision_record", project: "abc123",
			call: func(t *testing.T, _ *Server, s *mcp.ClientSession, tags []string) *mcp.CallToolResult {
				return callTool(t, s, "ghost_decision_record", map[string]any{
					"project_id": "test-project", "title": "a decision with a long tag",
					"decision": "cut long tags", "rationale": "a tag is a short label",
					"tags": tags,
				})
			},
		},
	}
}

func TestEveryTagWriterChecksTheUncutTagForATokenBeforeCuttingIt(t *testing.T) {
	for _, w := range tagWriters() {
		t.Run(w.name, func(t *testing.T) {
			srv, session := newCapSession(t)
			tag, token := tokenPastTagCap()
			res := w.call(t, srv, session, []string{"ops", tag})
			text := resultText(res)
			if !res.IsError {
				t.Fatalf("%s accepted a tag holding a token past byte %d, want refused: %q", w.name, tagMaxLen, text)
			}
			if !strings.Contains(text, "in tags[1]") {
				t.Errorf("%s refusal must name the tag by position (tags[1]): %q", w.name, text)
			}
			if strings.Contains(text, token) {
				t.Errorf("%s refusal printed the token back: %q", w.name, text)
			}
			if got := countProjectMemories(t, srv, w.project); got != w.seeded {
				t.Errorf("%s refused the tag but %s holds %d memory row(s), want %d", w.name, w.project, got, w.seeded)
			}
			if got := storedTags(t, srv, w.project); len(got) != 0 {
				t.Errorf("%s refused the tag but stored tags %v", w.name, got)
			}
			if w.name == "ghost_decision_record" {
				decisions, err := srv.store.ListDecisions(context.Background(), w.project, "", 10)
				if err != nil {
					t.Fatalf("ListDecisions: %v", err)
				}
				if len(decisions) != 0 {
					t.Errorf("a refused decision record still wrote %d decision row(s)", len(decisions))
				}
			}
		})
	}
}

func TestEveryTagWriterStillCutsALongTagWithNoToken(t *testing.T) {
	for _, w := range tagWriters() {
		t.Run(w.name, func(t *testing.T) {
			srv, session := newCapSession(t)
			long := strings.Repeat("c", 100)
			res := w.call(t, srv, session, []string{long})
			if res.IsError {
				t.Fatalf("%s refused a 100-byte tag with no token: %q", w.name, resultText(res))
			}
			got := storedTags(t, srv, w.project)
			want := strings.Repeat("c", tagMaxLen)
			if len(got) != 1 || got[0] != want {
				t.Errorf("%s stored tags %v, want the tag cut to %d bytes", w.name, got, tagMaxLen)
			}
		})
	}
}
