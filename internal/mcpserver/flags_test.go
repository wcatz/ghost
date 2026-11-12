package mcpserver

// #648 slice 2's MCP surface: `ghost_memory_flag`.

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/memory"
)

const flagReasonMarker = "ZZREASONNEVERLEAVESSTOREZZ"

func flagCount(t *testing.T, srv *Server, projectID, memoryID string) int {
	t.Helper()
	store, ok := srv.store.(*memory.Store)
	if !ok {
		t.Fatalf("server store is %T, want *memory.Store", srv.store)
	}
	id, _, err := store.ResolveProject(context.Background(), projectID)
	if err != nil || id == "" {
		t.Fatalf("ResolveProject(%q): %v", projectID, err)
	}
	ev, err := store.FlagsByMemory(context.Background(), id)
	if err != nil {
		t.Fatalf("FlagsByMemory: %v", err)
	}
	return ev[memoryID].Flagged
}

func seedFlagMemory(t *testing.T, session *mcp.ClientSession, projectID, content string) string {
	t.Helper()
	return parseToolID(t, callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": projectID,
		"content":    content,
		"category":   "fact",
	}))
}

func parseToolID(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	text := resultText(res)
	if id, ok := extractID(text); ok && id != "" {
		return id
	}
	for _, field := range strings.Fields(strings.TrimRight(text, ". \n")) {
		if len(field) == 32 && isLowerHex(field) {
			return field
		}
	}
	t.Fatalf("the save answer carries no memory id: %q", text)
	return ""
}

func isLowerHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func TestMemoryFlagIsRegisteredAsATool(t *testing.T) {
	_, session := newCapSession(t)
	tools, err := session.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range tools.Tools {
		if tool.Name != "ghost_memory_flag" {
			continue
		}
		if tool.Title == "" || tool.Description == "" {
			t.Errorf("ghost_memory_flag is registered with no title or description: %+v", tool)
		}
		if tool.Annotations == nil {
			t.Fatal("ghost_memory_flag has no annotations; a client has nothing to decide auto-approve from")
		}
		if tool.Annotations.DestructiveHint != nil && *tool.Annotations.DestructiveHint {
			t.Error("ghost_memory_flag is marked destructive; it appends an objection and deletes nothing")
		}
		if tool.Annotations.IdempotentHint {
			t.Error("ghost_memory_flag is marked idempotent; two flags on one memory are two objections, " +
				"and a client that skips the second would drop it")
		}
		return
	}
	t.Fatal("ghost_memory_flag is not registered as a tool")
}

func TestMemoryFlagWritesAndAnswersWithCountAndIdOnly(t *testing.T) {
	srv, session := newCapSession(t)
	const project = "test-project"
	id := seedFlagMemory(t, session, project, "the relay listens on port 2222 in staging")

	res := callTool(t, session, "ghost_memory_flag", map[string]any{
		"project_id": project,
		"memory_id":  id,
		"kind":       "wrong",
		"reason":     flagReasonMarker + " port 2222 was decommissioned in June",
	})
	text := resultText(res)
	if res.IsError {
		t.Fatalf("ghost_memory_flag was refused: %s", text)
	}
	if !strings.Contains(text, id) {
		t.Errorf("the answer does not name the memory it flagged: %q", text)
	}
	if strings.Contains(text, flagReasonMarker) {
		t.Errorf("the tool echoed the reason back into the caller's context: %q", text)
	}
	if n := flagCount(t, srv, project, id); n != 1 {
		t.Errorf("the store reports %d flag(s) for the memory, want 1", n)
	}

	res2 := callTool(t, session, "ghost_memory_flag", map[string]any{
		"project_id": project,
		"memory_id":  id,
		"kind":       "stale",
		"reason":     "the queue moved to the worker pool",
	})
	if text2 := resultText(res2); res2.IsError {
		t.Fatalf("the second flag was refused: %s", text2)
	} else if strings.Contains(text2, "the queue moved") {
		t.Errorf("the second flag echoed its reason too: %q", text2)
	}
	if n := flagCount(t, srv, project, id); n != 2 {
		t.Errorf("the store reports %d flag(s) after two calls, want 2", n)
	}
	mems, err := srv.store.GetByIDs(context.Background(), []string{id})
	if err != nil || len(mems) != 1 {
		t.Fatalf("GetByIDs: %v (%d rows)", err, len(mems))
	}
	if mems[0].ResolvedAt != nil {
		t.Errorf("flagging a memory resolved it: %v", *mems[0].ResolvedAt)
	}
}

func TestMemoryFlagRefusals(t *testing.T) {
	const reason = "this claims a port that no longer exists"
	overlong := strings.Repeat("r", memory.FlagReasonMax+1)

	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"unknown memory id", map[string]any{
			"project_id": "test-project", "memory_id": "ffffffffffffffffffffffffffffffff",
			"kind": "wrong", "reason": reason,
		}},
		{"memory in another project", map[string]any{
			"project_id": "test-project", "memory_id": "REPLACED",
			"kind": "wrong", "reason": reason,
		}},
		{"kind the tool does not define", map[string]any{
			"project_id": "test-project", "memory_id": "REPLACED",
			"kind": "useful", "reason": reason,
		}},
		{"no reason", map[string]any{
			"project_id": "test-project", "memory_id": "REPLACED",
			"kind": "stale", "reason": "",
		}},
		{"reason past the bound", map[string]any{
			"project_id": "test-project", "memory_id": "REPLACED",
			"kind": "stale", "reason": overlong,
		}},
		{"a credential-shaped reason", map[string]any{
			"project_id": "test-project", "memory_id": "REPLACED",
			"kind": "wrong", "reason": "the token is ghp_" + strings.Repeat("a", 36),
		}},
		{"no project", map[string]any{
			"memory_id": "REPLACED", "kind": "wrong", "reason": reason,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, session := newCapSession(t)
			mine := seedFlagMemory(t, session, "test-project", "the backup runs at 02:00 UTC")
			theirs := seedFlagMemory(t, session, "other-project", "the restore point is kept for a week")

			args := map[string]any{}
			for k, v := range tc.args {
				args[k] = v
			}
			if id, _ := args["memory_id"].(string); id == "REPLACED" {
				if tc.name == "memory in another project" {
					args["memory_id"] = theirs
					args["project_id"] = "test-project" // theirs belongs to other-project
				} else {
					args["memory_id"] = mine
				}
			}

			text := callToolErr(t, session, "ghost_memory_flag", args)
			if text == "" {
				t.Fatalf("the tool returned no message for a request it refused (%+v)", args)
			}
			if strings.Contains(text, "ghp_") {
				t.Errorf("the refusal echoed the credential: %q", text)
			}
			for _, p := range []string{"test-project", "other-project"} {
				id := mine
				if p == "other-project" {
					id = theirs
				}
				if n := flagCount(t, srv, p, id); n != 0 {
					t.Errorf("a refused flag left %d row(s) under %s", n, p)
				}
			}
		})
	}
}
