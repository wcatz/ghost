package mcpserver

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// bulkyContent is one row's worth of stored text: unique per index, so the save
// path's near-duplicate folding cannot collapse the fixture, and long enough that
// a handful of rows already exceed the response byte cap. The shared `entry`
// token is the one ghost_search_all matches on.
func bulkyContent(i int, noun string) string {
	return fmt.Sprintf("entry %02d %s%s", i, noun,
		strings.Repeat(fmt.Sprintf("tok%02d ", i), 1100))
}

// dumpedCut is the number a trimmed answer's notice reports, or -1 when the
// answer carries no notice at all.
var dumpedCut = regexp.MustCompile(`\((\d+) rows? (?:was|were) cut from the end of this answer`)

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

// TestCapRowsToResponseBudgetIsByteIdenticalWhenItFits: the budget only ever
// REMOVES rows. An answer that already fits is the uncut render, byte for byte,
// and the exact-fit boundary counts as fitting.
func TestCapRowsToResponseBudgetIsByteIdenticalWhenItFits(t *testing.T) {
	rows := []string{"- [fact] `a` first row\n", "- [fact] `b` second row\n"}
	want := strings.Join(rows, "")
	for _, budget := range []int{len(want), len(want) + 1000} {
		got, cut := capRowsToResponseBudget("", rows, budget, "Advise.")
		if got != want || cut != 0 {
			t.Errorf("budget %d: got cut=%d, bytes %d; want the byte-identical render cut=0, bytes %d",
				budget, cut, len(got), len(want))
		}
	}
}

// TestCapRowsToResponseBudgetCutsWholeRowsFromTheEnd: one byte under the fit
// drops the LAST row and only that row, and the notice says how many went and
// names the budget.
func TestCapRowsToResponseBudgetCutsWholeRowsFromTheEnd(t *testing.T) {
	// Rows big enough that dropping one leaves room for the notice, so the cut
	// reaches the budget rather than hitting the framing ceiling.
	rows := []string{
		"- [fact] `a` first row " + strings.Repeat("A", 1000) + "\n",
		"- [fact] `b` second row " + strings.Repeat("B", 1000) + "\n",
	}
	want := strings.Join(rows, "")
	got, cut := capRowsToResponseBudget("", rows, len(want)-1, "Advise.")
	if cut != 1 {
		t.Fatalf("cut = %d, want 1: one byte under the all-rows render must drop exactly the last row", cut)
	}
	if !strings.HasPrefix(got, rows[0]) {
		t.Errorf("the FIRST row must survive; got %q", tail(got, 200))
	}
	if strings.Contains(got, "second row") {
		t.Errorf("the last row must be the one cut; got %q", tail(got, 400))
	}
	if !strings.Contains(got, "1 row was cut from the end of this answer") {
		t.Errorf("the notice does not report the single cut row: %q", got)
	}
	if !strings.Contains(got, fmt.Sprintf("%d-byte response budget", len(want)-1)) {
		t.Errorf("the notice does not name the budget it was fitted to: %q", got)
	}
	if !strings.Contains(got, "Advise.") {
		t.Errorf("the notice does not carry the caller's advice: %q", got)
	}
	if len(got) > len(want)-1 {
		t.Errorf("the fitted answer is %d bytes against a %d-byte budget", len(got), len(want)-1)
	}
}

// TestCapRowsToResponseBudgetKeepsRowsWhenTheFramingIsTheExcess: when even the
// all-cut framing plus its notice cannot fit, cutting rows buys nothing and the
// answer keeps them rather than answering with nothing — the assembler's
// framing-ceiling rule.
func TestCapRowsToResponseBudgetKeepsRowsWhenTheFramingIsTheExcess(t *testing.T) {
	framing := strings.Repeat("F", 200)
	rows := []string{"row one\n", "row two\n"}
	want := framing + strings.Join(rows, "")
	got, cut := capRowsToResponseBudget(framing, rows, 10, "Advise.")
	if got != want || cut != 0 {
		t.Errorf("a framing that exceeds the budget by itself must keep every row: got cut=%d bytes=%d, want cut=0 bytes=%d",
			cut, len(got), len(want))
	}
}

// TestMemoriesListStaysInsideTheResponseBudget: a browse is bounded by `limit`
// alone, so 30 memories at the store's content cap are hundreds of KB. The tool
// must fit them to the search response cap, keep whole rows, and say how many it
// cut.
func TestMemoriesListStaysInsideTheResponseBudget(t *testing.T) {
	_, session := newCapSession(t)
	const n = 30
	for i := range n {
		saveMemory(t, session, bulkyContent(i, "mem"))
	}

	res := callTool(t, session, "ghost_memories_list", map[string]any{
		"project_id": "test-project",
		"limit":      100,
	})
	if res.IsError {
		t.Fatalf("list failed: %s", resultText(res))
	}
	out := resultText(res)

	assertWithinBudget(t, out)
	kept := countItemLines(out)
	cut := cutCount(t, out)
	if cut < 1 {
		t.Fatalf("no cut was reported, so the fixture did not exceed the cap: bytes=%d cut=%d", len(out), cut)
	}
	if kept < 1 {
		t.Fatalf("every row was cut, so the answer is rows and nothing else: %s", tail(out, 300))
	}
	if kept+cut != n {
		t.Errorf("%d rows kept + %d cut = %d, want %d: the notice count must match the rows gone",
			kept, cut, kept+cut, n)
	}
}

// TestSearchAllStaysInsideTheResponseBudget is the cross-project sibling of the
// browse test: the same cap, the same whole-row cut, over a keyword search.
func TestSearchAllStaysInsideTheResponseBudget(t *testing.T) {
	_, session := newCapSession(t)
	const n = 30
	for i := range n {
		saveMemory(t, session, bulkyContent(i, "mem"))
	}

	res := callTool(t, session, "ghost_search_all", map[string]any{
		"query": "entry",
		"limit": 100,
	})
	if res.IsError {
		t.Fatalf("search_all failed: %s", resultText(res))
	}
	out := resultText(res)

	assertWithinBudget(t, out)
	kept := countItemLines(out)
	cut := cutCount(t, out)
	if cut < 1 {
		t.Fatalf("no cut was reported, so the fixture did not exceed the cap: bytes=%d cut=%d", len(out), cut)
	}
	if kept < 1 {
		t.Fatalf("every row was cut: %s", tail(out, 300))
	}
	if kept+cut != n {
		t.Errorf("%d rows kept + %d cut = %d, want %d", kept, cut, kept+cut, n)
	}
}

// TestTaskListStaysInsideTheResponseBudget: 30 tasks with 8 KB descriptions are
// one 482 KB answer bounded by `limit` alone. Whole tasks are cut, and the notice
// names how many.
func TestTaskListStaysInsideTheResponseBudget(t *testing.T) {
	_, session := newCapSession(t)
	const n = 30
	for i := range n {
		res := callTool(t, session, "ghost_task_create", map[string]any{
			"project_id":  "test-project",
			"title":       fmt.Sprintf("bulk task %02d", i),
			"description": bulkyContent(i, "task"),
		})
		if res.IsError {
			t.Fatalf("task_create %d: %s", i, resultText(res))
		}
	}

	res := callTool(t, session, "ghost_task_list", map[string]any{
		"project_id": "test-project",
		"limit":      100,
	})
	if res.IsError {
		t.Fatalf("task_list failed: %s", resultText(res))
	}
	out := resultText(res)

	assertWithinBudget(t, out)
	kept := countTaskRows(out)
	cut := cutCount(t, out)
	if cut < 1 {
		t.Fatalf("no cut was reported, so the fixture did not exceed the cap: bytes=%d cut=%d", len(out), cut)
	}
	if kept < 1 {
		t.Fatalf("every task was cut: %s", tail(out, 300))
	}
	if kept+cut != n {
		t.Errorf("%d tasks kept + %d cut = %d, want %d", kept, cut, kept+cut, n)
	}
}

// TestDecisionsListStaysInsideTheResponseBudget: 30 decisions with 8 KB decision
// text. Each decision is one whole row — the `### …` record — and the explainer
// is framing that always survives.
func TestDecisionsListStaysInsideTheResponseBudget(t *testing.T) {
	_, session := newCapSession(t)
	const n = 30
	for i := range n {
		res := callTool(t, session, "ghost_decision_record", map[string]any{
			"project_id": "test-project",
			"title":      fmt.Sprintf("bulk decision %02d", i),
			"decision":   bulkyContent(i, "decide"),
			"rationale":  "short rationale",
		})
		if res.IsError {
			t.Fatalf("decision_record %d: %s", i, resultText(res))
		}
	}

	res := callTool(t, session, "ghost_decisions_list", map[string]any{
		"project_id": "test-project",
		"limit":      100,
	})
	if res.IsError {
		t.Fatalf("decisions_list failed: %s", resultText(res))
	}
	out := resultText(res)

	assertWithinBudget(t, out)
	if !strings.Contains(out, dataDelimiterNote) {
		t.Errorf("the explainer is framing and must survive a trim: %s", tail(out, 300))
	}
	kept := strings.Count(out, "\n### ")
	if strings.HasPrefix(out, "### ") {
		kept++
	}
	cut := cutCount(t, out)
	if cut < 1 {
		t.Fatalf("no cut was reported, so the fixture did not exceed the cap: bytes=%d cut=%d", len(out), cut)
	}
	if kept < 1 {
		t.Fatalf("every decision was cut: %s", tail(out, 300))
	}
	if kept+cut != n {
		t.Errorf("%d decisions kept + %d cut = %d, want %d", kept, cut, kept+cut, n)
	}
}

// TestProjectContextStaysInsideTheResponseBudget: `ghost_project_context` with
// the same 30-row corpus. The block's headings, pinned lines and learned summary
// are framing; the memory rows are cut whole.
func TestProjectContextStaysInsideTheResponseBudget(t *testing.T) {
	_, session := newCapSession(t)
	const n = 30
	for i := range n {
		saveMemory(t, session, bulkyContent(i, "ctx"))
	}

	res := callTool(t, session, "ghost_project_context", map[string]any{
		"project_id": "test-project",
		"limit":      100,
	})
	if res.IsError {
		t.Fatalf("project_context failed: %s", resultText(res))
	}
	out := resultText(res)

	assertWithinBudget(t, out)
	if !strings.Contains(out, "## Memories") {
		t.Errorf("the Memories heading is framing and must survive a trim: %s", tail(out, 300))
	}
	kept := countItemLines(out)
	cut := cutCount(t, out)
	if cut < 1 {
		t.Fatalf("no cut was reported, so the fixture did not exceed the cap: bytes=%d cut=%d", len(out), cut)
	}
	if kept < 1 {
		t.Fatalf("every memory row was cut: %s", tail(out, 300))
	}
	if kept+cut != n {
		t.Errorf("%d rows kept + %d cut = %d, want %d", kept, cut, kept+cut, n)
	}
}

// TestProjectContextAsOfStaysInsideTheResponseBudget is the historical sibling:
// the as_of branch builds its own block, and it must obey the same budget.
func TestProjectContextAsOfStaysInsideTheResponseBudget(t *testing.T) {
	_, session := newCapSession(t)
	const n = 30
	for i := range n {
		saveMemory(t, session, bulkyContent(i, "ctx"))
	}

	res := callTool(t, session, "ghost_project_context", map[string]any{
		"project_id": "test-project",
		"limit":      100,
		"as_of":      asOfToolFuture,
	})
	if res.IsError {
		t.Fatalf("project_context as_of failed: %s", resultText(res))
	}
	out := resultText(res)

	assertWithinBudget(t, out)
	if !strings.Contains(out, "historical read") {
		t.Errorf("the as_of qualifier is framing and must survive a trim: %s", tail(out, 300))
	}
	if !strings.Contains(out, "## Memories") {
		t.Errorf("the Memories heading is framing and must survive a trim: %s", tail(out, 300))
	}
	kept := countItemLines(out)
	cut := cutCount(t, out)
	if cut < 1 {
		t.Fatalf("no cut was reported, so the fixture did not exceed the cap: bytes=%d cut=%d", len(out), cut)
	}
	if kept < 1 {
		t.Fatalf("every historical row was cut: %s", tail(out, 300))
	}
	if kept+cut != n {
		t.Errorf("%d rows kept + %d cut = %d, want %d", kept, cut, kept+cut, n)
	}
}

// TestReadToolsKeepSmallAnswersByteIdentical is the other half of the contract:
// a small answer carries no notice and every row, so the budget changes nothing
// until it has to.
func TestReadToolsKeepSmallAnswersByteIdentical(t *testing.T) {
	srv, session := newCapSession(t)
	ctx := context.Background()
	for i := range 3 {
		saveMemory(t, session, fmt.Sprintf("small durable fact number %d", i))
	}
	wantRows, err := srv.store.ListMemories(ctx, "abc123", "", "", 100)
	if err != nil {
		t.Fatalf("ListMemories: %v", err)
	}
	want := formatMemories(wantRows)

	out := resultText(callTool(t, session, "ghost_memories_list", map[string]any{
		"project_id": "test-project",
		"limit":      100,
	}))
	if out != want {
		t.Errorf("a small listing must be the byte-identical uncapped render.\nwant:\n%s\ngot:\n%s", want, out)
	}
	if cutCount(t, out) != -1 {
		t.Errorf("a small listing must carry no cut notice: %s", out)
	}
}

// assertWithinBudget fails when the delivered text exceeds the shipped cap.
func assertWithinBudget(t *testing.T, out string) {
	t.Helper()
	if len(out) > searchResponseMaxBytes {
		t.Errorf("the answer is %d bytes, over the %d-byte cap: a listing that can fill a harness's "+
			"context window is one nobody can use", len(out), searchResponseMaxBytes)
	}
}

// countTaskRows counts the task header lines (`- [status] Pn \`id\` …`), one per
// task, and not the indented description lines that belong to them.
func countTaskRows(out string) int {
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "- [") {
			n++
		}
	}
	return n
}
