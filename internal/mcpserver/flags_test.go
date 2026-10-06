package mcpserver

// #648 slice 2's MCP surface: `ghost_memory_flag`.
//
// The tool is the only way an agent can record that it believes a memory is
// wrong or stale, and everything about it is deliberately small: one memory, one
// of two kinds, one short reason. What the ANSWER carries is the property worth
// testing — the reason is written to the store and is not echoed back, because
// this tool's result lands in the same agent context the reason came from and a
// round trip would put it there twice.

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/memory"
)

// flagReasonMarker is unmistakable. Its presence in a tool RESULT is the
// failure: the reason is free text an agent wrote, and echoing it back would
// put it into the context of every later turn.
const flagReasonMarker = "ZZREASONNEVERLEAVESSTOREZZ"

// flagCount is the flag count this store reports for one memory, read through
// the same evidence path resolve and reflect read it through — so the test that
// a write took is a test that the FEATURE can see it, not that a row exists.
func flagCount(t *testing.T, srv *Server, projectID, memoryID string) int {
	t.Helper()
	// The concrete store, not the capability interface: evidence is not on
	// provider.MemoryStore, and a test that read it through a widening
	// assertion would be measuring a fake.
	store, ok := srv.store.(*memory.Store)
	if !ok {
		t.Fatalf("server store is %T, want *memory.Store", srv.store)
	}
	// Evidence is keyed on the project's ID and the caller holds its name —
	// the same resolution the tool itself does, so this reads the figure the
	// next resolve pass will be handed rather than an empty map looked up
	// under a key no row carries.
	id, _, err := store.ResolveProject(context.Background(), projectID)
	if err != nil || id == "" {
		t.Fatalf("ResolveProject(%q): %v", projectID, err)
	}
	ev, err := store.UsefulnessByMemory(context.Background(), id)
	if err != nil {
		t.Fatalf("UsefulnessByMemory: %v", err)
	}
	return ev[memoryID].Flagged
}

// seedFlagMemory saves a memory through the live save tool and returns its id.
func seedFlagMemory(t *testing.T, session *mcp.ClientSession, projectID, content string) string {
	t.Helper()
	return parseToolID(t, callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": projectID,
		"content":    content,
		"category":   "fact",
	}))
}

// parseToolID pulls a memory id out of a save answer without assuming the whole
// sentence around it — the save tool's wording is its own contract, and this
// test is about the flag tool.
func parseToolID(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	text := resultText(res)
	if id, ok := extractID(text); ok && id != "" {
		return id
	}
	// A save answer that names no id cannot be flagged by this fixture, and
	// failing here says so rather than producing a confusing refusal below.
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

// TestMemoryFlagIsRegisteredAsATool: a helper nothing calls is not a
// capability, and the annotations are a claim a CLIENT acts on.
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
		// Not destructive: the flag adds a row and changes no memory.
		if tool.Annotations.DestructiveHint != nil && *tool.Annotations.DestructiveHint {
			t.Error("ghost_memory_flag is marked destructive; it appends an objection and deletes nothing")
		}
		// NOT idempotent: a second flag on the same memory is a second objection
		// with its own reason. The SDK types this hint as a plain bool with
		// omitempty and the spec defaults it to false, so `true` is the only
		// spelling a client reads as "call again and nothing happens" — and a
		// client that believed it would skip the second flag an agent filed.
		if tool.Annotations.IdempotentHint {
			t.Error("ghost_memory_flag is marked idempotent; two flags on one memory are two objections, " +
				"and a client that skips the second would drop it")
		}
		return
	}
	t.Fatal("ghost_memory_flag is not registered as a tool")
}

// TestMemoryFlagWritesAndAnswersWithCountAndIdOnly: the happy path, and the
// bound on what comes back.
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
	if !strings.Contains(text, "1") {
		t.Errorf("the answer carries no count: %q", text)
	}
	if strings.Contains(text, flagReasonMarker) {
		t.Errorf("the tool echoed the reason back into the caller's context: %q", text)
	}
	if n := flagCount(t, srv, project, id); n != 1 {
		t.Errorf("the store reports %d flag(s) for the memory, want 1", n)
	}

	// A second flag appends, and the answer's count moves with it.
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
	// Nothing else moved: a flag is not a resolve, a delete or a demotion.
	mems, err := srv.store.GetByIDs(context.Background(), []string{id})
	if err != nil || len(mems) != 1 {
		t.Fatalf("GetByIDs: %v (%d rows)", err, len(mems))
	}
	if mems[0].ResolvedAt != nil {
		t.Errorf("flagging a memory resolved it: %v", *mems[0].ResolvedAt)
	}
}

// TestMemoryFlagRefusals: every reason the tool says no, and that saying no
// wrote nothing.
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
			// A real memory in test-project, and a second project holding its own.
			// The cross-project case needs a memory that EXISTS somewhere, or the
			// refusal would be "unknown id" and the test would agree for the
			// wrong reason.
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
			// Nothing was written, under either project.
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
