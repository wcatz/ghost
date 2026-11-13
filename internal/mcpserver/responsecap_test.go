package mcpserver

import (
	"fmt"
	"maps"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/assemble"
)

func bulkyContent(i int, noun string) string {
	return fmt.Sprintf("entry %02d %s%s", i, noun, strings.Repeat(fmt.Sprintf("tok%02d ", i), 1100))
}

var dumpedCut = regexp.MustCompile(`\((\d+) rows? (?:was|were) cut from (?:the end of )?this answer`)

func cutCount(t *testing.T, out string) int {
	t.Helper()
	m := dumpedCut.FindStringSubmatch(out)
	if m == nil {
		return -1
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("cut count %q is not a number: %v", m[1], err)
	}
	return n
}

func assertWithinBudget(t *testing.T, out string) {
	t.Helper()
	if len(out) > searchResponseMaxBytes {
		t.Errorf("answer is %d bytes, over the %d-byte cap", len(out), searchResponseMaxBytes)
	}
}

func TestCapRowsToResponseBudget(t *testing.T) {
	rows := []string{"- [fact] `a` first row\n", "- [fact] `b` second row\n"}
	want := strings.Join(rows, "")
	for _, budget := range []int{len(want), len(want) + 1000} {
		if got, cut := capRowsToResponseBudget("", rows, budget, "Advise."); got != want || cut != 0 {
			t.Fatalf("budget %d: cut=%d bytes=%d", budget, cut, len(got))
		}
	}
	big := []string{"- [fact] `a` first row " + strings.Repeat("A", 1000) + "\n", "- [fact] `b` second row " + strings.Repeat("B", 1000) + "\n"}
	full := strings.Join(big, "")
	got, cut := capRowsToResponseBudget("", big, len(full)-1, "Advise.")
	if cut != 1 || !strings.HasPrefix(got, big[0]) || strings.Contains(got, "second row") || len(got) > len(full)-1 {
		t.Fatalf("cut=%d len=%d", cut, len(got))
	}
	framing := strings.Repeat("F", 200)
	rows2 := []string{"row one\n", "row two\n"}
	if got, cut := capRowsToResponseBudget(framing, rows2, 10, "Advise."); got != framing+strings.Join(rows2, "") || cut != 0 {
		t.Fatalf("framing excess: cut=%d", cut)
	}
}

func TestCapResponsePartsNeverDropsEveryRow(t *testing.T) {
	row := "### big\n" + strings.Repeat("D", 16000) + "\n\n"
	got, cut := capResponseParts([]responsePart{{text: "## Decisions\n\n"}, {text: row, cuttable: true}}, 500, "Advise.")
	if cut != 0 || !strings.Contains(got, row) {
		t.Fatalf("cut=%d hasRow=%v", cut, strings.Contains(got, row))
	}
}

func TestReadToolsStayWithinBudget(t *testing.T) {
	_, session := newCapSession(t)
	for i := 0; i < 8; i++ {
		callTool(t, session, "ghost_memory_save", map[string]any{"project_id": "test-project", "content": bulkyContent(i, "memory"), "category": "fact"})
	}
	for _, args := range []map[string]any{
		{"project_id": "test-project"},
		{"project_id": "test-project", "query": "entry"},
	} {
		name := "ghost_memories_list"
		if _, ok := args["query"]; ok {
			name = "ghost_memory_search"
		}
		assertWithinBudget(t, resultText(callTool(t, session, name, args)))
	}
	assertWithinBudget(t, resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": "test-project"})))
	for i := 0; i < 5; i++ {
		callTool(t, session, "ghost_task_create", map[string]any{"project_id": "test-project", "title": "task " + strings.Repeat("t", 200) + fmt.Sprint(i), "description": strings.Repeat("d", 2000)})
	}
	assertWithinBudget(t, resultText(callTool(t, session, "ghost_task_list", map[string]any{"project_id": "test-project"})))
	for i := 0; i < 5; i++ {
		callTool(t, session, "ghost_decision_record", map[string]any{"project_id": "test-project", "title": "decision " + fmt.Sprint(i), "decision": strings.Repeat("D", 2000), "rationale": strings.Repeat("R", 2000)})
	}
	assertWithinBudget(t, resultText(callTool(t, session, "ghost_decisions_list", map[string]any{"project_id": "test-project"})))
}

func TestOversizedRowsAreKeptAndNoMatchAbstains(t *testing.T) {
	_, session := newCapSession(t)
	res := callTool(t, session, "ghost_decision_record", map[string]any{"project_id": "test-project", "title": "big", "decision": strings.Repeat("D", 8000), "rationale": strings.Repeat("R", 8000)})
	if res.IsError {
		t.Fatalf("record: %s", resultText(res))
	}
	out := resultText(callTool(t, session, "ghost_decisions_list", map[string]any{"project_id": "test-project"}))
	if !strings.Contains(out, "big") || strings.Contains(out, "No decisions recorded yet.") {
		t.Fatalf("oversized decision missing: %q", out[:min(200, len(out))])
	}
	_, session = newCapSession(t)
	huge := "entry huge " + strings.Repeat("payload ", 3500)
	if res := callTool(t, session, "ghost_memory_save", map[string]any{"project_id": "test-project", "content": huge, "category": "fact"}); res.IsError {
		t.Fatalf("save: %s", resultText(res))
	}
	out = resultText(callTool(t, session, "ghost_memory_search", map[string]any{"project_id": "test-project", "query": "entry"}))
	if !strings.Contains(out, "entry huge") {
		t.Fatalf("oversized memory missing: %q", out[:min(200, len(out))])
	}
	_, session = newCapSession(t)
	callTool(t, session, "ghost_memory_save", map[string]any{"project_id": "test-project", "content": "an unrelated memory about giraffes", "category": "fact"})
	out = resultText(callTool(t, session, "ghost_memory_search", map[string]any{"project_id": "test-project", "query": "zebra"}))
	if !strings.Contains(out, "[ghost:outcome=empty") || strings.Contains(out, "shown in full") {
		t.Fatalf("unexpected abstention: %q", out)
	}
}

// TestMemorySearchKeepsAMemoryLargerThanTheBudget is #1483's tool-level
// regression. A memory saved whole at the store's content cap whose line breaks
// fold to the 3-byte `⏎` renders past the 16,000-byte search budget, and search
// used to drop it and abstain with all_over_budget, `limit: 1` included, while
// ghost_memories_list returned the same row whole. Search now keeps its first
// row the same way, with a notice, on a current and on an as_of read.
func TestMemorySearchKeepsAMemoryLargerThanTheBudget(t *testing.T) {
	srv, session := newCapSession(t)
	content := "zebra" + strings.Repeat("\nx", 3997)
	if len(content) != 7999 {
		t.Fatalf("fixture is %d bytes, want 7999", len(content))
	}
	if _, stored := saveAndFetchContent(t, srv, session, content); stored != content {
		t.Fatalf("precondition: the content was not stored whole (%d of %d bytes)", len(stored), len(content))
	}

	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"no limit", map[string]any{}},
		{"limit 1", map[string]any{"limit": 1}},
		{"as_of", map[string]any{"as_of": "2035-01-01T00:00:00Z"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]any{"project_id": "test-project", "query": "zebra"}
			maps.Copy(args, tc.args)
			res := callTool(t, session, "ghost_memory_search", args)
			if res.IsError {
				t.Fatalf("search failed: %s", resultText(res))
			}
			out := resultText(res)
			if len(out) <= searchResponseMaxBytes {
				t.Fatalf("the answer is %d bytes, inside the budget: the fixture never put the row over it", len(out))
			}
			if !strings.Contains(out, assemble.Data(content)) {
				t.Errorf("the memory is not in the answer whole:\n%s", tail(out, 400))
			}
			if strings.Contains(out, "outcome=empty") || strings.Contains(out, "all_over_budget") {
				t.Errorf("the answer abstains over a row it could return:\n%s", tail(out, 400))
			}
			if !strings.Contains(out, "The first row alone is larger than the 16000-byte response budget, so it is shown in full") {
				t.Errorf("the notice does not say the oversized row was kept whole:\n%s", tail(out, 400))
			}
		})
	}
}
