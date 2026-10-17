package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/memory"
)

// The write tools cut oversized caller text to its cap (8000 bytes for
// content, descriptions, decisions and rationales; 300 for titles and each
// alternative; 20 alternatives). The token guard has to see the text BEFORE
// that cut: a token-shaped value placed past the cap would otherwise be cut
// out of sight, and the call would answer with a truncation warning instead
// of a refusal. These tests place the token just past each cap and require a
// refusal that names the field, with nothing written.

// tokenPastCap is 8000 'a', a space, then a token-shaped value, so the token
// lies entirely past the 8000-byte content cap.
func tokenPastCap() string {
	return strings.Repeat("a", memory.MaxContentLen) + " ghp_" + strings.Repeat("b", 36)
}

// tokenPastTitleCap is the same shape past the 300-byte title cap.
func tokenPastTitleCap() string {
	return strings.Repeat("a", maxTitleLen) + " ghp_" + strings.Repeat("b", 36)
}

// oversizedClean is past the content cap with no token in it.
func oversizedClean() string {
	return strings.Repeat("a", memory.MaxContentLen+500)
}

// requireRefusedNaming asserts the call came back as an error result whose
// text names the refused field.
func requireRefusedNaming(t *testing.T, session *mcp.ClientSession, tool string, args map[string]any, field string) {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: protocol error %v, want an IsError result", tool, err)
	}
	text := resultText(res)
	if !res.IsError {
		t.Fatalf("%s with a token past the cap in %s was accepted, want refused: %q", tool, field, text)
	}
	if !strings.Contains(text, "in "+field) {
		t.Fatalf("%s refusal must name the field %q: %q", tool, field, text)
	}
}

func requireNoTasks(t *testing.T, srv *Server) {
	t.Helper()
	tasks, err := srv.store.ListTasks(context.Background(), "abc123", "", 50)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("a refused task create stored %d task(s): %+v", len(tasks), tasks)
	}
}

func requireNoDecisionWrite(t *testing.T, srv *Server) {
	t.Helper()
	ctx := context.Background()
	decisions, err := srv.store.ListDecisions(ctx, "abc123", "", 50)
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	if len(decisions) != 0 {
		t.Fatalf("a refused decision record stored %d decision(s)", len(decisions))
	}
	mems, err := srv.store.ListMemories(ctx, "abc123", "", "", 50)
	if err != nil {
		t.Fatalf("ListMemories: %v", err)
	}
	if len(mems) != 0 {
		t.Fatalf("a refused decision record stored %d companion memor(ies)", len(mems))
	}
}

func requireNoMemories(t *testing.T, srv *Server, projectID string) {
	t.Helper()
	mems, err := srv.store.ListMemories(context.Background(), projectID, "", "", 50)
	if err != nil {
		t.Fatalf("ListMemories(%s): %v", projectID, err)
	}
	if len(mems) != 0 {
		t.Fatalf("a refused save stored %d memor(ies) in %s", len(mems), projectID)
	}
}

func TestTaskCreate_TokenPastCapIsRefused(t *testing.T) {
	t.Run("description", func(t *testing.T) {
		srv, session := newCapSession(t)
		requireRefusedNaming(t, session, "ghost_task_create", map[string]any{
			"project_id":  "test-project",
			"title":       "task with an oversized description",
			"description": tokenPastCap(),
		}, "description")
		requireNoTasks(t, srv)
	})
	t.Run("title", func(t *testing.T) {
		srv, session := newCapSession(t)
		requireRefusedNaming(t, session, "ghost_task_create", map[string]any{
			"project_id": "test-project",
			"title":      tokenPastTitleCap(),
		}, "title")
		requireNoTasks(t, srv)
	})
	t.Run("control: oversized clean text is accepted with its warning", func(t *testing.T) {
		srv, session := newCapSession(t)
		res := callTool(t, session, "ghost_task_create", map[string]any{
			"project_id":  "test-project",
			"title":       strings.Repeat("a", maxTitleLen+50),
			"description": oversizedClean(),
		})
		text := resultText(res)
		if res.IsError {
			t.Fatalf("oversized clean task was refused: %q", text)
		}
		for _, want := range []string{"task title was truncated at 300 bytes", "task description was truncated at 8000 bytes"} {
			if !strings.Contains(text, want) {
				t.Errorf("answer must carry %q: %q", want, text)
			}
		}
		tasks, err := srv.store.ListTasks(context.Background(), "abc123", "", 50)
		if err != nil || len(tasks) != 1 {
			t.Fatalf("ListTasks: err=%v n=%d, want 1", err, len(tasks))
		}
	})
}

func TestTaskUpdate_TokenPastCapIsRefused(t *testing.T) {
	srv, session := newCapSession(t)
	ctx := context.Background()
	res := callTool(t, session, "ghost_task_create", map[string]any{
		"project_id":  "test-project",
		"title":       "task to update",
		"description": "original description",
	})
	id, ok := extractID(resultText(res))
	if !ok {
		t.Fatalf("create answer carries no id: %q", resultText(res))
	}

	requireRefusedNaming(t, session, "ghost_task_update", map[string]any{
		"task_id":     id,
		"description": tokenPastCap(),
	}, "description")
	task, err := srv.store.GetTask(ctx, id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Description != "original description" {
		t.Fatalf("a refused update changed the description to len %d", len(task.Description))
	}

	res = callTool(t, session, "ghost_task_update", map[string]any{
		"task_id":     id,
		"description": oversizedClean(),
	})
	text := resultText(res)
	if res.IsError {
		t.Fatalf("oversized clean update was refused: %q", text)
	}
	if !strings.Contains(text, "task description was truncated at 8000 bytes") {
		t.Fatalf("control update must warn of the cut: %q", text)
	}
}

func TestDecisionRecord_TokenPastCapIsRefused(t *testing.T) {
	base := func() map[string]any {
		return map[string]any{
			"project_id": "test-project",
			"title":      "decision title",
			"decision":   "decided",
			"rationale":  "because",
		}
	}
	twentyOneAlts := func() []string {
		alts := make([]string, 21)
		for i := range alts {
			alts[i] = "alternative"
		}
		alts[20] = "ghp_" + strings.Repeat("b", 36)
		return alts
	}
	cases := []struct {
		name  string
		field string
		set   func(args map[string]any)
	}{
		{"decision", "decision", func(a map[string]any) { a["decision"] = tokenPastCap() }},
		{"rationale", "rationale", func(a map[string]any) { a["rationale"] = tokenPastCap() }},
		{"title", "title", func(a map[string]any) { a["title"] = tokenPastTitleCap() }},
		{"alternative past 300 bytes", "alternatives[0]", func(a map[string]any) {
			a["alternatives"] = []string{tokenPastTitleCap()}
		}},
		{"alternative past the 20-entry bound", "alternatives[20]", func(a map[string]any) {
			a["alternatives"] = twentyOneAlts()
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, session := newCapSession(t)
			args := base()
			tc.set(args)
			requireRefusedNaming(t, session, "ghost_decision_record", args, tc.field)
			requireNoDecisionWrite(t, srv)
		})
	}

	t.Run("control: oversized clean text is accepted with its warning", func(t *testing.T) {
		srv, session := newCapSession(t)
		alts := make([]string, 21)
		for i := range alts {
			alts[i] = "alternative"
		}
		alts[0] = strings.Repeat("a", maxTitleLen+50)
		res := callTool(t, session, "ghost_decision_record", map[string]any{
			"project_id":   "test-project",
			"title":        strings.Repeat("a", maxTitleLen+50),
			"decision":     oversizedClean(),
			"rationale":    oversizedClean(),
			"alternatives": alts,
		})
		text := resultText(res)
		if res.IsError {
			t.Fatalf("oversized clean decision was refused: %q", text)
		}
		for _, want := range []string{
			"decision title was truncated at 300 bytes",
			"decision and rationale text was truncated at 8000 bytes",
			"decision alternatives were truncated",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("answer must carry %q: %q", want, text)
			}
		}
		decisions, err := srv.store.ListDecisions(context.Background(), "abc123", "", 50)
		if err != nil || len(decisions) != 1 {
			t.Fatalf("ListDecisions: err=%v n=%d, want 1", err, len(decisions))
		}
	})
}

func TestMemorySave_TokenPastCapIsRefused(t *testing.T) {
	srv, session := newCapSession(t)
	requireRefusedNaming(t, session, "ghost_memory_save", map[string]any{
		"project_id": "test-project",
		"content":    tokenPastCap(),
		"category":   "fact",
	}, "content")
	requireNoMemories(t, srv, "abc123")

	resp, stored := saveAndFetchContent(t, srv, session, oversizedClean())
	if !strings.Contains(resp, truncationWarningLiteral) {
		t.Fatalf("control save must warn of the cut: %q", resp)
	}
	if !strings.HasSuffix(stored, truncationMarkerLiteral) {
		t.Fatalf("control save must store the marker; tail %q", tail(stored, 60))
	}
}

func TestMemoryUpdate_TokenPastCapIsRefused(t *testing.T) {
	srv, session := newCapSession(t)
	ctx := context.Background()
	res := callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": "test-project",
		"content":    "original content",
		"category":   "fact",
	})
	id, ok := extractID(resultText(res))
	if !ok {
		t.Fatalf("save answer carries no id: %q", resultText(res))
	}

	requireRefusedNaming(t, session, "ghost_memory_update", map[string]any{
		"project_id": "test-project",
		"memory_id":  id,
		"content":    tokenPastCap(),
	}, "content")
	mems, err := srv.store.GetByIDs(ctx, []string{id})
	if err != nil || len(mems) != 1 {
		t.Fatalf("GetByIDs: err=%v n=%d", err, len(mems))
	}
	if mems[0].Content != "original content" {
		t.Fatalf("a refused update changed the content to len %d", len(mems[0].Content))
	}

	res = callTool(t, session, "ghost_memory_update", map[string]any{
		"project_id": "test-project",
		"memory_id":  id,
		"content":    oversizedClean(),
	})
	text := resultText(res)
	if res.IsError {
		t.Fatalf("oversized clean update was refused: %q", text)
	}
	if !strings.Contains(text, truncationWarningLiteral) {
		t.Fatalf("control update must warn of the cut: %q", text)
	}
}

func TestSaveGlobal_TokenPastCapIsRefused(t *testing.T) {
	srv, session := newCapSession(t)
	requireRefusedNaming(t, session, "ghost_save_global", map[string]any{
		"content": tokenPastCap(),
	}, "content")
	requireNoMemories(t, srv, memory.GlobalProjectID)

	res := callTool(t, session, "ghost_save_global", map[string]any{
		"content": oversizedClean(),
	})
	text := resultText(res)
	if res.IsError {
		t.Fatalf("oversized clean global save was refused: %q", text)
	}
	if !strings.Contains(text, truncationWarningLiteral) {
		t.Fatalf("control global save must warn of the cut: %q", text)
	}
	mems, err := srv.store.ListMemories(context.Background(), memory.GlobalProjectID, "", "", 50)
	if err != nil || len(mems) != 1 {
		t.Fatalf("ListMemories(_global): err=%v n=%d, want 1", err, len(mems))
	}
}
