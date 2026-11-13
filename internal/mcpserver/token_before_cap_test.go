package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/memory"
)

func tokenPastCap() string {
	return strings.Repeat("a", memory.MaxContentLen) + " ghp_" + strings.Repeat("b", 36)
}
func tokenPastTitleCap() string {
	return strings.Repeat("a", maxTitleLen) + " ghp_" + strings.Repeat("b", 36)
}
func oversizedClean() string { return strings.Repeat("a", memory.MaxContentLen+500) }

func requireRefusedNaming(t *testing.T, session *mcp.ClientSession, tool string, args map[string]any, field string) {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	text := resultText(res)
	if !res.IsError || !strings.Contains(text, "in "+field) {
		t.Fatalf("%s %s: %q", tool, field, text)
	}
}

func TestTokenPastCapRefusals(t *testing.T) {
	{
		srv, session := newCapSession(t)
		requireRefusedNaming(t, session, "ghost_task_create", map[string]any{"project_id": "test-project", "title": "t", "description": tokenPastCap()}, "description")
		if tasks, _ := srv.store.ListTasks(context.Background(), "abc123", "", 50); len(tasks) != 0 {
			t.Fatal("task stored")
		}
		requireRefusedNaming(t, session, "ghost_task_create", map[string]any{"project_id": "test-project", "title": tokenPastTitleCap()}, "title")
	}
	{
		srv, session := newCapSession(t)
		res := callTool(t, session, "ghost_task_create", map[string]any{"project_id": "test-project", "title": "task to update"})
		id, _ := extractID(resultText(res))
		requireRefusedNaming(t, session, "ghost_task_update", map[string]any{"task_id": id, "description": tokenPastCap()}, "description")
		task, _ := srv.store.GetTask(context.Background(), id)
		if task.Description != "" {
			t.Fatalf("description changed: %q", task.Description)
		}
	}
	{
		base := map[string]any{"project_id": "test-project", "title": "decision title", "decision": "decided", "rationale": "because"}
		alts := make([]string, 21)
		for i := range alts {
			alts[i] = "alternative"
		}
		alts[20] = "ghp_" + strings.Repeat("b", 36)
		for _, tc := range []struct {
			field string
			args  map[string]any
		}{
			{"decision", map[string]any{"project_id": "test-project", "title": "t", "decision": tokenPastCap(), "rationale": "r"}},
			{"rationale", map[string]any{"project_id": "test-project", "title": "t", "decision": "d", "rationale": tokenPastCap()}},
			{"title", map[string]any{"project_id": "test-project", "title": tokenPastTitleCap(), "decision": "d", "rationale": "r"}},
			{"alternatives[0]", map[string]any{"project_id": "test-project", "title": "t", "decision": "d", "rationale": "r", "alternatives": []string{tokenPastTitleCap()}}},
			{"alternatives[20]", map[string]any{"project_id": "test-project", "title": "t", "decision": "d", "rationale": "r", "alternatives": alts}},
		} {
			srv, session := newCapSession(t)
			_ = base
			requireRefusedNaming(t, session, "ghost_decision_record", tc.args, tc.field)
			if decs, _ := srv.store.ListDecisions(context.Background(), "abc123", "", 50); len(decs) != 0 {
				t.Fatalf("%s stored decision", tc.field)
			}
			if mems, _ := srv.store.ListMemories(context.Background(), "abc123", "", "", 50); len(mems) != 0 {
				t.Fatalf("%s stored memory", tc.field)
			}
		}
	}
	{
		srv, session := newCapSession(t)
		requireRefusedNaming(t, session, "ghost_memory_save", map[string]any{"project_id": "test-project", "content": tokenPastCap(), "category": "fact"}, "content")
		if mems, _ := srv.store.ListMemories(context.Background(), "abc123", "", "", 50); len(mems) != 0 {
			t.Fatal("memory stored")
		}
	}
	{
		srv, session := newCapSession(t)
		res := callTool(t, session, "ghost_memory_save", map[string]any{"project_id": "test-project", "content": "original content", "category": "fact"})
		id, _ := extractID(resultText(res))
		requireRefusedNaming(t, session, "ghost_memory_update", map[string]any{"project_id": "test-project", "memory_id": id, "content": tokenPastCap()}, "content")
		mems, _ := srv.store.GetByIDs(context.Background(), []string{id})
		if len(mems) != 1 || mems[0].Content != "original content" {
			t.Fatalf("content changed: %+v", mems)
		}
	}
	{
		srv, session := newCapSession(t)
		requireRefusedNaming(t, session, "ghost_save_global", map[string]any{"content": tokenPastCap()}, "content")
		if mems, _ := srv.store.ListMemories(context.Background(), memory.GlobalProjectID, "", "", 50); len(mems) != 0 {
			t.Fatal("global memory stored")
		}
	}
}

func TestTokenPastCapControlsStillTruncate(t *testing.T) {
	{
		_, session := newCapSession(t)
		res := callTool(t, session, "ghost_task_create", map[string]any{"project_id": "test-project", "title": strings.Repeat("a", maxTitleLen+50), "description": oversizedClean()})
		text := resultText(res)
		if res.IsError || !strings.Contains(text, "task title was truncated at 300 bytes") || !strings.Contains(text, "task description was truncated at 8000 bytes") {
			t.Fatalf("task control: %q", text)
		}
	}
	{
		_, session := newCapSession(t)
		res := callTool(t, session, "ghost_memory_save", map[string]any{"project_id": "test-project", "content": oversizedClean(), "category": "fact"})
		if res.IsError || !strings.Contains(resultText(res), truncationWarningLiteral) {
			t.Fatalf("save control: %q", resultText(res))
		}
	}
	{
		_, session := newCapSession(t)
		res := callTool(t, session, "ghost_save_global", map[string]any{"content": oversizedClean()})
		if res.IsError || !strings.Contains(resultText(res), truncationWarningLiteral) {
			t.Fatalf("global control: %q", resultText(res))
		}
	}
}
