package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// badWriteText is the free-text a write tool must refuse, one row per class:
// whitespace that reads as blank, invisible FORMAT characters that render as
// nothing, and a NUL byte.
//
// A value that is blank is not merely empty: "   " and "\n\t" are both arguments
// an agent produces when it means to pass something and passes nothing, and the
// old `== ""` checks accepted every one of them. The zero-width row is the class
// TrimSpace alone misses — U+200B ZERO WIDTH SPACE, U+2060 WORD JOINER and U+FEFF
// ZERO WIDTH NO-BREAK SPACE are category Cf, not spaces — so a value made only of
// them also rendered as nothing. The NUL case is the one that survives JSON: a
// client can send it as `\u0000` inside a string, so it arrives at the handler as
// a real NUL and must be refused there. Invalid UTF-8 is deliberately absent here
// — the JSON transport coerces an invalid byte to U+FFFD before the handler sees
// it — and is covered against the helper itself in
// TestValidateRequiredTextRefusesInvalidUTF8.
var badWriteText = map[string]string{
	"spaces":          "   ",
	"tab and newline": "\n\t",
	"zero-width only": "\u200b\u2060\ufeff",
	"a NUL byte":      "a\x00b",
}

// TestSaveToolsRefuseBlankOrMalformedContent is #1038 for the two tools that
// create a memory from free text.
//
// The assertion is on the ANSWER naming the field, not on a particular sentence:
// the requirement is "refuse", and a test pinned to wording would break on a
// wording change that still refuses. What must not happen is the old behaviour,
// where the save succeeded and the blank text became a row.
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
				got := callToolErr(t, session, tool.name, args)
				if !strings.Contains(got, "content") {
					t.Errorf("%s: the refusal does not name the content field: %s", tool.name, got)
				}
				assertNoMemory(t, srv, tool.projects...)
			})
		}
	}
}

// TestMemoryUpdateRefusesBlankOrMalformedContentAndLeavesTheRowIntact is the
// half of #1038 the issue calls out by name: a refused update must not be a
// partial write. The row is seeded with real text, the edit carries blank text,
// and the stored content is read back and must be byte-identical to what it was.
func TestMemoryUpdateRefusesBlankOrMalformedContentAndLeavesTheRowIntact(t *testing.T) {
	for shape, bad := range badWriteText {
		t.Run(shape, func(t *testing.T) {
			srv, session := newCapSession(t)

			const original = "the text that must survive a refused edit"
			id, err := srv.store.Create(context.Background(), "abc123", memory.Memory{
				Category: "fact", Content: original, Source: "mcp", Importance: 0.7,
			})
			if err != nil {
				t.Fatalf("seed memory: %v", err)
			}

			got := callToolErr(t, session, "ghost_memory_update", map[string]any{
				"project_id": "test-project",
				"memory_id":  id,
				"content":    bad,
			})
			if !strings.Contains(got, "content") {
				t.Errorf("the update refusal does not name the content field: %s", got)
			}

			mems, err := srv.store.GetByIDs(context.Background(), []string{id})
			if err != nil || len(mems) != 1 {
				t.Fatalf("GetByIDs(%s): err=%v n=%d", id, err, len(mems))
			}
			if mems[0].Content != original {
				t.Errorf("a refused update changed the stored content: got %q, want %q", mems[0].Content, original)
			}
		})
	}
}

// TestTitleWritersRefuseBlankOrMalformedTitles is the title half of #1038, on
// both tools that take a title.
func TestTitleWritersRefuseBlankOrMalformedTitles(t *testing.T) {
	for _, tool := range []struct {
		name string
		args map[string]any
	}{
		{"ghost_task_create", map[string]any{"project_id": "test-project"}},
		{"ghost_decision_record", map[string]any{
			"project_id": "test-project", "decision": "we chose it", "rationale": "because",
		}},
	} {
		for shape, bad := range badWriteText {
			t.Run(tool.name+"/"+shape, func(t *testing.T) {
				srv, session := newCapSession(t)
				args := map[string]any{"title": bad}
				for k, v := range tool.args {
					args[k] = v
				}
				got := callToolErr(t, session, tool.name, args)
				if !strings.Contains(got, "title") {
					t.Errorf("%s: the refusal does not name the title field: %s", tool.name, got)
				}
				assertNoTaskAndNoDecision(t, srv, "test-project")
			})
		}
	}
}

// TestDecisionRecordRefusesBlankOrMalformedDecisionAndRationale is the rest of
// #1038 on ghost_decision_record: a blank `decision` or `rationale` is the same
// class as a blank `title`, and the old `== ""` check on each stored "   "
// verbatim. `title` is a real value here, so the refusal is about the field under
// test and not about a blank title firing first.
func TestDecisionRecordRefusesBlankOrMalformedDecisionAndRationale(t *testing.T) {
	for _, field := range []string{"decision", "rationale"} {
		for shape, bad := range badWriteText {
			t.Run(field+"/"+shape, func(t *testing.T) {
				srv, session := newCapSession(t)
				args := map[string]any{
					"project_id": "test-project",
					"title":      "we chose it",
					"decision":   "we chose it",
					"rationale":  "because",
				}
				args[field] = bad
				got := callToolErr(t, session, "ghost_decision_record", args)
				if !strings.Contains(got, field) {
					t.Errorf("the refusal does not name the %s field: %s", field, got)
				}
				assertNoTaskAndNoDecision(t, srv, "test-project")
			})
		}
	}
}

// TestMemorySaveRefusesBlankOrOversizedProjectID is the identifier half of #1038.
//
// Two classes, one assertion: nothing is opened. A blank id is the "  " case the
// issue names; an over-long id is the 100000-character case that made
// ghost_list_projects render about 300 KB. The companion test below pins that an
// id at the cap is still accepted, so this test cannot pass by refusing every
// project_id.
func TestMemorySaveRefusesBlankOrOversizedProjectID(t *testing.T) {
	for shape, bad := range map[string]string{
		"spaces":      "   ",
		"tab":         "\t",
		"newline":     "\n",
		"zero-width":  "\u200b\u2060\ufeff",
		"oversized":   strings.Repeat("a", maxProjectIDLen+1),
		"way too big": strings.Repeat("a", 100000),
	} {
		t.Run(shape, func(t *testing.T) {
			srv, session := newCapSession(t)
			got := callToolErr(t, session, "ghost_memory_save", map[string]any{
				"project_id": bad, "content": "a claim", "category": "fact",
			})
			if !strings.Contains(got, "project_id") {
				t.Errorf("the refusal does not name the project_id field: %s", got)
			}
			if projectExists(t, srv, bad) {
				t.Errorf("a save opened a project under the refused id %q", bad)
			}
			assertNoMemory(t, srv, bad)
		})
	}
}

// TestProjectIDAtTheCapIsStillAccepted is the other half, and it exists so the
// refusal above cannot be satisfied by a cap of zero. A path-shaped id is used so
// the name-shaped checkout binding is not what this test measures.
func TestProjectIDAtTheCapIsStillAccepted(t *testing.T) {
	srv, session := newCapSession(t)
	atCap := "/" + strings.Repeat("a", maxProjectIDLen-1)
	if len(atCap) != maxProjectIDLen {
		t.Fatalf("fixture is %d bytes, want exactly the cap %d", len(atCap), maxProjectIDLen)
	}
	out := resultText(callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": atCap, "content": "a claim", "category": "fact",
	}))
	if !strings.Contains(out, "Memory saved") {
		t.Fatalf("a project_id at the cap was refused: %s", out)
	}
	if !projectExists(t, srv, atCap) {
		t.Errorf("the project_id at the cap did not create a project")
	}
}

// assertNoMemory fails when a memory exists in any of the named projects or in
// _global, which is what a refused content or project_id write must leave behind:
// nothing. The projects are the CALLER's because each tool writes to its own
// project — "test-project", or the refused id in the identifier test — and a
// helper pinned to a fixed control id would pass without ever inspecting the
// project the write would have landed in.
func assertNoMemory(t *testing.T, srv *Server, projectIDs ...string) {
	t.Helper()
	for _, projectID := range append(projectIDs, memory.GlobalProjectID) {
		if n := countProjectMemories(t, srv, projectID); n != 0 {
			t.Errorf("a refused write stored %d memory(ies) in %s", n, projectID)
		}
	}
}

// assertNoTaskAndNoDecision fails when a refused title, decision or rationale
// produced a task or a decision in any of the named projects, which is the write
// the old `== ""` checks let through. The projects are the caller's for the same
// reason assertNoMemory takes them.
func assertNoTaskAndNoDecision(t *testing.T, srv *Server, projectIDs ...string) {
	t.Helper()
	for _, projectID := range projectIDs {
		tasks, err := srv.store.ListTasks(context.Background(), projectID, "", 10)
		if err != nil {
			t.Fatalf("ListTasks(%q): %v", projectID, err)
		}
		if len(tasks) != 0 {
			t.Errorf("a refused write created %d task(s) in %s", len(tasks), projectID)
		}
		decisions, err := srv.store.ListDecisions(context.Background(), projectID, "", 10)
		if err != nil {
			t.Fatalf("ListDecisions(%q): %v", projectID, err)
		}
		if len(decisions) != 0 {
			t.Errorf("a refused write created %d decision(s) in %s", len(decisions), projectID)
		}
	}
}

// TestValidateRequiredTextRefusesInvalidUTF8 covers the class the JSON transport
// cannot deliver: encoding/json coerces an invalid byte to U+FFFD before the
// handler sees it, so the boundary tests above cannot exercise this arm. The
// helper is where the rule lives, so it is tested directly.
func TestValidateRequiredTextRefusesInvalidUTF8(t *testing.T) {
	for name, value := range map[string]string{
		"a lone continuation byte": "a\xffb",
		"a truncated sequence":     "a\xc3",
	} {
		t.Run(name, func(t *testing.T) {
			err := validateRequiredText("content", value)
			if err == nil {
				t.Fatalf("validateRequiredText accepted invalid UTF-8 %q", value)
			}
			if !strings.Contains(err.Error(), "content") || !strings.Contains(err.Error(), "UTF-8") {
				t.Errorf("the refusal must name the field and the class: %v", err)
			}
			// The invalid bytes must not be replayed into the error string.
			if strings.Contains(err.Error(), value) {
				t.Errorf("the refusal echoes the invalid value: %q", err.Error())
			}
		})
	}
}

// TestValidateRequiredTextRefusesZeroWidthOnly pins the class TrimSpace misses:
// the invisible FORMAT characters U+200B, U+2060 and U+FEFF — and U+00AD SOFT
// HYPHEN, the same category — are not spaces, so a value made only of one of them
// is blank to a reader but was not blank to a `== ""` or a TrimSpace check. Each
// is refused on its own; the tool-level rows in badWriteText cover the three
// together.
func TestValidateRequiredTextRefusesZeroWidthOnly(t *testing.T) {
	for name, value := range map[string]string{
		"a zero width space":    "\u200b",
		"a word joiner":         "\u2060",
		"a zero width no-break": "\ufeff",
		"a soft hyphen":         "\u00ad",
	} {
		t.Run(name, func(t *testing.T) {
			err := validateRequiredText("content", value)
			if err == nil {
				t.Fatalf("validateRequiredText accepted the invisible value %q", value)
			}
			if !strings.Contains(err.Error(), "content") {
				t.Errorf("the refusal must name the field: %v", err)
			}
		})
	}
}

// TestValidateRequiredTextAcceptsOrdinaryText is the false-positive half: the
// rule refuses blank and malformed values and nothing else. Leading and trailing
// whitespace around real text, an internal newline, and non-ASCII characters are
// all ordinary content and must pass.
func TestValidateRequiredTextAcceptsOrdinaryText(t *testing.T) {
	for _, value := range []string{
		"a claim",
		"  padded claim  ",
		"line one\nline two",
		"日本語のメモ",
		"a\u200bzero-width\u2060 beside real text",
	} {
		if err := validateRequiredText("content", value); err != nil {
			t.Errorf("validateRequiredText refused ordinary text %q: %v", value, err)
		}
	}
}

// TestValidateProjectIDArgumentBoundaryAndShape pins the two halves of the
// project_id rule that the boundary tests exercise through the tool: exactly the
// cap is accepted, one byte more is refused, and whitespace inside a path is not
// blank.
func TestValidateProjectIDArgumentBoundaryAndShape(t *testing.T) {
	atCap := strings.Repeat("a", maxProjectIDLen)
	if err := validateProjectIDArgument(atCap); err != nil {
		t.Errorf("project_id at the cap was refused: %v", err)
	}
	overCap := strings.Repeat("a", maxProjectIDLen+1)
	if err := validateProjectIDArgument(overCap); err == nil {
		t.Errorf("project_id one byte over the cap was accepted")
	}
	for _, ok := range []string{"/Users/w/My Projects/ghost", "proj name"} {
		if err := validateProjectIDArgument(ok); err != nil {
			t.Errorf("a project_id with a space inside was refused: %q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "   ", "\t", "\u200b\u2060\ufeff", "a\x00b"} {
		if err := validateProjectIDArgument(bad); err == nil {
			t.Errorf("a blank or malformed project_id was accepted: %q", bad)
		}
	}
}
