package mcpserver

// The tool annotations are a claim a CLIENT acts on: a host decides from them
// whether to auto-approve a call and whether it may retry one after a timeout.
// The MCP spec defines idempotentHint as "no additional effect on its
// environment when repeated with the same arguments", so a tool advertised as
// idempotent that stores a row per call makes every retry add a duplicate.
// These tests read the hints through a real tools/list and pin them against
// what a repeated call does.

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/memory"
)

type wantAnnotations struct {
	readOnly    bool
	destructive bool // the DestructiveHint pointer's value; nil is not expected for a writer
	idempotent  bool
	why         string
}

// listedAnnotations returns the annotations of every tool as tools/list
// reports them.
func listedAnnotations(t *testing.T) map[string]*mcp.ToolAnnotations {
	t.Helper()
	_, session := newCapSession(t)
	tools, err := session.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	got := map[string]*mcp.ToolAnnotations{}
	for _, tool := range tools.Tools {
		got[tool.Name] = tool.Annotations
	}
	return got
}

// TestToolAnnotationsPinned pins readOnly/destructive/idempotent/openWorld for
// every tool. A new tool fails here until it is given a row and a reason.
func TestToolAnnotationsPinned(t *testing.T) {
	want := map[string]wantAnnotations{
		// Read-only: nothing a user would confirm. DestructiveHint and
		// IdempotentHint are meaningless alongside readOnly and stay unset.
		"ghost_memory_search":   {readOnly: true, why: "reads; its audit row is bookkeeping, see the comment at the annotation"},
		"ghost_project_context": {readOnly: true, why: "readOnlyHint is owned by a separate issue and is not changed here"},
		"ghost_memories_list":   {readOnly: true, why: "lists rows"},
		"ghost_search_all":      {readOnly: true, why: "reads"},
		"ghost_task_list":       {readOnly: true, why: "lists rows"},
		"ghost_health":          {readOnly: true, why: "reports statistics"},
		"ghost_list_projects":   {readOnly: true, why: "lists rows"},
		"ghost_decisions_list":  {readOnly: true, why: "lists rows"},
		// Writers that add a row per call: a repeat is another row, so not idempotent.
		"ghost_memory_save":     {why: "a repeat stores a second row and raises the first row's importance"},
		"ghost_save_global":     {why: "a repeat stores a second row and raises the first row's importance"},
		"ghost_task_create":     {why: "a repeat creates a second task"},
		"ghost_decision_record": {why: "a repeat records a second decision"},
		"ghost_memory_flag":     {why: "two flags are two objections"},
		"ghost_memory_update":   {why: "every edit appends a history row and moves updated_at"},
		"ghost_resolve":         {why: "each run classifies afresh through the harness and is billed"},
		"ghost_memory_promote":  {why: "a repeat is refused rather than converging; not claimed idempotent"},
		// Writers whose repeat converges on the same state.
		"ghost_memory_pin":    {idempotent: true, why: "sets pinned to a value"},
		"ghost_task_update":   {idempotent: true, why: "sets fields to values"},
		"ghost_task_complete": {idempotent: true, why: "an already-done task keeps its completed_at"},
		"ghost_resolve_mark":  {destructive: true, idempotent: true, why: "an already-resolved memory is a reported no-op"},
		// Destructive.
		"ghost_memory_delete":  {destructive: true, why: "permanent delete; a repeat takes the purge path"},
		"ghost_project_delete": {destructive: true, why: "permanent delete"},
		"ghost_link_withdraw":  {destructive: true, why: "a repeat is an error, not a no-op"},
	}
	got := listedAnnotations(t)
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("tool %s has no pinned annotations; add a row with the reason", name)
		}
	}
	for name, w := range want {
		a, ok := got[name]
		if !ok {
			t.Errorf("tool %s is not listed", name)
			continue
		}
		if a == nil {
			t.Errorf("%s has no annotations", name)
			continue
		}
		if a.ReadOnlyHint != w.readOnly {
			t.Errorf("%s readOnlyHint = %v, want %v (%s)", name, a.ReadOnlyHint, w.readOnly, w.why)
		}
		if a.IdempotentHint != w.idempotent {
			t.Errorf("%s idempotentHint = %v, want %v (%s)", name, a.IdempotentHint, w.idempotent, w.why)
		}
		if a.OpenWorldHint == nil || *a.OpenWorldHint {
			t.Errorf("%s openWorldHint is not false; Ghost talks to a local store only", name)
		}
		if !w.readOnly {
			if a.DestructiveHint == nil {
				t.Errorf("%s leaves destructiveHint unset, which the spec reads as true", name)
			} else if *a.DestructiveHint != w.destructive {
				t.Errorf("%s destructiveHint = %v, want %v (%s)", name, *a.DestructiveHint, w.destructive, w.why)
			}
		}
	}
}

// TestSaveToolsRepeatAddsARowSoAreNotIdempotent proves the behaviour the hint
// is checked against: an identical repeat stores another row.
func TestSaveToolsRepeatAddsARowSoAreNotIdempotent(t *testing.T) {
	for _, tc := range []struct {
		tool, project string
	}{
		{"ghost_memory_save", "test-project"},
		{"ghost_save_global", "_global"},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			srv, session := newCapSession(t)
			args := map[string]any{
				"content":  "the relay listens on port 2222 in staging",
				"category": "fact",
			}
			if tc.tool == "ghost_memory_save" {
				args["project_id"] = tc.project
			}
			callTool(t, session, tc.tool, args)
			callTool(t, session, tc.tool, args)
			pid, _, err := srv.store.ResolveProject(context.Background(), tc.project)
			if err != nil || pid == "" {
				t.Fatalf("ResolveProject(%s) = %q, %v", tc.project, pid, err)
			}
			mems, err := srv.store.ListMemories(context.Background(), pid, "", "", 50)
			if err != nil {
				t.Fatalf("ListMemories: %v", err)
			}
			if len(mems) < 2 {
				t.Fatalf("an identical repeat of %s stored %d row(s); the premise that it adds a row no longer holds, revisit the hint", tc.tool, len(mems))
			}
			if a := listedAnnotations(t)[tc.tool]; a == nil || a.IdempotentHint {
				t.Errorf("%s repeats into another row but advertises idempotentHint", tc.tool)
			}
		})
	}
}

// TestMemoryUpdateRepeatAppendsHistoryRow: an identical repeat of an edit
// appends another history row, so it has an additional effect.
func TestMemoryUpdateRepeatAppendsHistoryRow(t *testing.T) {
	srv, session := newCapSession(t)
	const project = "test-project"
	id := seedFlagMemory(t, session, project, "the relay listens on port 2222 in staging")
	hist, ok := srv.store.(interface {
		MemoryHistory(context.Context, string, int) ([]memory.HistoryEntry, error)
	})
	if !ok {
		t.Fatal("the test store does not expose MemoryHistory")
	}
	args := map[string]any{"project_id": project, "memory_id": id, "importance": 0.9}
	callTool(t, session, "ghost_memory_update", args)
	first, err := hist.MemoryHistory(context.Background(), id, 100)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	callTool(t, session, "ghost_memory_update", args)
	second, err := hist.MemoryHistory(context.Background(), id, 100)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if len(second) <= len(first) {
		t.Fatalf("a repeated identical edit left history at %d rows; the premise no longer holds, revisit the hint", len(second))
	}
	if a := listedAnnotations(t)["ghost_memory_update"]; a == nil || a.IdempotentHint {
		t.Error("ghost_memory_update appends a history row per call but advertises idempotentHint")
	}
}
