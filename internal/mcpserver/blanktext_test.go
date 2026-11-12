package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

var badWriteText = map[string]string{
	"spaces": "   ", "tab and newline": "\n\t", "zero-width only": "\u200b\u2060\ufeff", "a NUL byte": "a\x00b",
}

func assertNoMemory(t *testing.T, srv *Server, projectIDs ...string) {
	t.Helper()
	for _, projectID := range append(projectIDs, memory.GlobalProjectID) {
		if n := countProjectMemories(t, srv, projectID); n != 0 {
			t.Fatalf("stored %d memories in %s", n, projectID)
		}
	}
}

func assertNoTaskAndNoDecision(t *testing.T, srv *Server, projectIDs ...string) {
	t.Helper()
	for _, projectID := range projectIDs {
		tasks, _ := srv.store.ListTasks(context.Background(), projectID, "", 10)
		decisions, _ := srv.store.ListDecisions(context.Background(), projectID, "", 10)
		if len(tasks) != 0 || len(decisions) != 0 {
			t.Fatalf("stored tasks=%d decisions=%d in %s", len(tasks), len(decisions), projectID)
		}
	}
}

// #1038
func TestSaveToolsRefuseBlankOrMalformedContent(t *testing.T) {
	for _, tool := range []struct {
		name     string
		args     map[string]any
		projects []string
	}{
		{"ghost_memory_save", map[string]any{"project_id": "test-project", "category": "fact"}, []string{"test-project"}},
		{"ghost_save_global", map[string]any{}, nil},
	} {
		for shape, bad := range badWriteText {
			t.Run(tool.name+"/"+shape, func(t *testing.T) {
				srv, session := newCapSession(t)
				args := map[string]any{"content": bad}
				for k, v := range tool.args {
					args[k] = v
				}
				if got := callToolErr(t, session, tool.name, args); !strings.Contains(got, "content") {
					t.Fatalf("refusal missing content: %s", got)
				}
				assertNoMemory(t, srv, tool.projects...)
			})
		}
	}
}

// #1038
func TestMemoryUpdateRefusesBlankOrMalformedContentAndLeavesTheRowIntact(t *testing.T) {
	for shape, bad := range badWriteText {
		t.Run(shape, func(t *testing.T) {
			srv, session := newCapSession(t)
			const original = "the text that must survive a refused edit"
			id, err := srv.store.Create(context.Background(), "abc123", memory.Memory{Category: "fact", Content: original, Source: "mcp", Importance: 0.7})
			if err != nil {
				t.Fatal(err)
			}
			if got := callToolErr(t, session, "ghost_memory_update", map[string]any{"project_id": "test-project", "memory_id": id, "content": bad}); !strings.Contains(got, "content") {
				t.Fatalf("refusal missing content: %s", got)
			}
			mems, _ := srv.store.GetByIDs(context.Background(), []string{id})
			if len(mems) != 1 || mems[0].Content != original {
				t.Fatalf("row changed: %+v", mems)
			}
		})
	}
}

// #1038
func TestTitleWritersRefuseBlankOrMalformedTitles(t *testing.T) {
	for _, tool := range []struct {
		name string
		args map[string]any
	}{
		{"ghost_task_create", map[string]any{"project_id": "test-project"}},
		{"ghost_decision_record", map[string]any{"project_id": "test-project", "decision": "we chose it", "rationale": "because"}},
	} {
		for shape, bad := range badWriteText {
			t.Run(tool.name+"/"+shape, func(t *testing.T) {
				srv, session := newCapSession(t)
				args := map[string]any{"title": bad}
				for k, v := range tool.args {
					args[k] = v
				}
				if got := callToolErr(t, session, tool.name, args); !strings.Contains(got, "title") {
					t.Fatalf("refusal missing title: %s", got)
				}
				assertNoTaskAndNoDecision(t, srv, "test-project")
			})
		}
	}
}

// #1038
func TestDecisionRecordRefusesBlankOrMalformedDecisionAndRationale(t *testing.T) {
	for _, field := range []string{"decision", "rationale"} {
		for shape, bad := range badWriteText {
			t.Run(field+"/"+shape, func(t *testing.T) {
				srv, session := newCapSession(t)
				args := map[string]any{"project_id": "test-project", "title": "we chose it", "decision": "we chose it", "rationale": "because"}
				args[field] = bad
				if got := callToolErr(t, session, "ghost_decision_record", args); !strings.Contains(got, field) {
					t.Fatalf("refusal missing %s: %s", field, got)
				}
				assertNoTaskAndNoDecision(t, srv, "test-project")
			})
		}
	}
}

// #1038
func TestMemorySaveRefusesBlankOrOversizedProjectID(t *testing.T) {
	for shape, bad := range map[string]string{"spaces": "   ", "tab": "\t", "newline": "\n", "zero-width": "\u200b\u2060\ufeff", "oversized": strings.Repeat("a", maxProjectIDLen+1), "way too big": strings.Repeat("a", 100000)} {
		t.Run(shape, func(t *testing.T) {
			srv, session := newCapSession(t)
			if got := callToolErr(t, session, "ghost_memory_save", map[string]any{"project_id": bad, "content": "a claim", "category": "fact"}); !strings.Contains(got, "project_id") {
				t.Fatalf("refusal missing project_id: %s", got)
			}
			if projectExists(t, srv, bad) {
				t.Fatalf("project created for %q", bad)
			}
			assertNoMemory(t, srv, bad)
		})
	}
}

func TestProjectIDAtTheCapIsStillAccepted(t *testing.T) {
	srv, session := newCapSession(t)
	atCap := "/" + strings.Repeat("a", maxProjectIDLen-1)
	out := resultText(callTool(t, session, "ghost_memory_save", map[string]any{"project_id": atCap, "content": "a claim", "category": "fact"}))
	if !strings.Contains(out, "Memory saved") || !projectExists(t, srv, atCap) {
		t.Fatalf("cap project_id not accepted: %s", out)
	}
}

func TestValidateRequiredTextAndProjectID(t *testing.T) {
	for _, value := range []string{"a\xffb", "a\xc3"} {
		err := validateRequiredText("content", value)
		if err == nil || !strings.Contains(err.Error(), "UTF-8") || strings.Contains(err.Error(), value) {
			t.Fatalf("bad utf8 refusal: %v", err)
		}
	}
	for _, value := range []string{"\u200b", "\u2060", "\ufeff", "\u00ad"} {
		if err := validateRequiredText("content", value); err == nil {
			t.Fatalf("accepted invisible %q", value)
		}
	}
	for _, value := range []string{"a claim", "  padded claim  ", "line one\nline two", "日本語のメモ", "a\u200bzero-width\u2060 beside real text"} {
		if err := validateRequiredText("content", value); err != nil {
			t.Fatalf("refused ordinary %q: %v", value, err)
		}
	}
	atCap := strings.Repeat("a", maxProjectIDLen)
	if err := validateProjectIDArgument(atCap); err != nil {
		t.Fatal(err)
	}
	if err := validateProjectIDArgument(strings.Repeat("a", maxProjectIDLen+1)); err == nil {
		t.Fatal("over cap accepted")
	}
	for _, ok := range []string{"/Users/w/My Projects/ghost", "proj name"} {
		if err := validateProjectIDArgument(ok); err != nil {
			t.Fatalf("refused %q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "   ", "\t", "\u200b\u2060\ufeff", "a\x00b"} {
		if err := validateProjectIDArgument(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
