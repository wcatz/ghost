package mcpserver

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/assemble"
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

// TestCapResponsePartsNeverDropsEveryRow is the regression for the one oversized
// row: a decision whose 8000-byte decision and rationale are clamped separately
// is about 16 KB, larger than the budget. Cutting it would leave framing and a
// notice with nothing to read, so the first row is kept whole and the notice
// says the budget could not be met.
func TestCapResponsePartsNeverDropsEveryRow(t *testing.T) {
	framing := "## Decisions\n\n"
	row := "### big\n" + strings.Repeat("D", 16000) + "\n\n"
	parts := []responsePart{
		{text: framing},
		{text: row, cuttable: true},
	}
	got, cut := capResponseParts(parts, 500, "Advise.")
	if !strings.Contains(got, row) {
		t.Fatalf("the only row was dropped, so the answer is unreadable: got cut=%d bytes=%d:\n%s",
			cut, len(got), got)
	}
	if !strings.HasPrefix(got, framing) {
		t.Errorf("the framing must lead the answer: %q", tail(got, 200))
	}
	if !strings.Contains(got, "shown in full") {
		t.Errorf("the notice does not say the oversized row was kept whole: %q", tail(got, 300))
	}
	if strings.Contains(got, "Ask for fewer results") {
		t.Errorf("the notice gives advice that cannot shrink one row: %q", tail(got, 300))
	}
}

// TestCapResponsePartsDoesNotClaimAFittingRowIsTooLarge: the protected row fits
// the budget and the framing plus the notice are what put the answer over. The
// row is still kept whole, and the notice, which is the assembler's shared
// sentence, says the row fits rather than that it alone is larger.
func TestCapResponsePartsDoesNotClaimAFittingRowIsTooLarge(t *testing.T) {
	framing := strings.Repeat("f", 300)
	row := strings.Repeat("r", 300)
	parts := []responsePart{
		{text: framing},
		{text: row, cuttable: true},
		{text: strings.Repeat("s", 300), cuttable: true},
	}
	got, cut := capResponseParts(parts, 500, "Advise.")
	if cut != 1 || !strings.Contains(got, row) {
		t.Fatalf("want the lower row cut and the first kept whole: cut=%d\n%s", cut, got)
	}
	if strings.Contains(got, "alone is larger") {
		t.Errorf("the notice claims a 300-byte row is larger than a 500-byte budget: %q", tail(got, 300))
	}
	want := "(1 row was cut from the end of this answer, and the first row fits the 500-byte response budget, " +
		"but not with the rest of this answer around it, so it is shown in full over the budget rather than dropped.)"
	if !strings.Contains(got, want) {
		t.Errorf("the notice is not %q: %q", want, tail(got, 300))
	}
}

// TestCapResponsePartsSaysAnOversizedRowIsLargerThanTheBudget is the other half
// of the case above: a protected row whose own text is over the budget keeps the
// "alone is larger" sentence, so a listing that always took the framing wording
// would fail here rather than tell a reader a 600-byte row fits a 500-byte budget.
func TestCapResponsePartsSaysAnOversizedRowIsLargerThanTheBudget(t *testing.T) {
	row := strings.Repeat("r", 600)
	parts := []responsePart{
		{text: strings.Repeat("f", 100)},
		{text: row, cuttable: true},
		{text: strings.Repeat("s", 300), cuttable: true},
	}
	got, cut := capResponseParts(parts, 500, "Advise.")
	if cut != 1 || !strings.Contains(got, row) {
		t.Fatalf("want the lower row cut and the oversized first row kept whole: cut=%d\n%s", cut, got)
	}
	if strings.Contains(got, "first row fits") {
		t.Errorf("the notice says a 600-byte row fits a 500-byte budget: %q", tail(got, 300))
	}
	if !strings.Contains(got, "the first row alone is larger than the 500-byte response budget") {
		t.Errorf("the notice does not say the first row alone is larger than the budget: %q", tail(got, 300))
	}
}

// TestCapResponsePartsCountsTheNoticeAgainstTheBudget closes a mutation gap: a
// pass that tested `total-removed <= budget` without the notice would return an
// answer whose rows fit but whose notice pushes it back over. The fixture makes
// the notice the difference — cutting one row leaves the rows exactly at the
// budget, so only counting the notice moves the pass to cut two.
func TestCapResponsePartsCountsTheNoticeAgainstTheBudget(t *testing.T) {
	advice := "Advise."
	r0 := strings.Repeat("A", 200)
	r1 := strings.Repeat("B", 300)
	r2 := strings.Repeat("C", 300)
	budget := len(r0) + len(r1) // exactly the first two rows, no room for a notice

	n1 := len(responseCapNotice(1, budget, advice))
	if len(r0)+len(r1)+n1 <= budget {
		t.Fatalf("precondition: one cut plus its notice must not fit (rows=%d notice=%d budget=%d)",
			len(r0)+len(r1), n1, budget)
	}
	n2 := len(responseCapNotice(2, budget, advice))
	if len(r0)+n2 > budget {
		t.Fatalf("precondition: two cuts plus their notice must fit (row=%d notice=%d budget=%d)",
			len(r0), n2, budget)
	}
	parts := []responsePart{
		{text: r0, cuttable: true},
		{text: r1, cuttable: true},
		{text: r2, cuttable: true},
	}
	got, cut := capResponseParts(parts, budget, advice)
	if cut != 2 {
		t.Fatalf("cut = %d, want 2: the notice pushed the first cut over the budget, so a second row must go", cut)
	}
	if len(got) > budget {
		t.Errorf("the answer is %d bytes against a %d-byte budget: the notice was not counted against it",
			len(got), budget)
	}
	if !strings.Contains(got, r0) || strings.Contains(got, r1) || strings.Contains(got, r2) {
		t.Errorf("the kept prefix is not exactly the first row: %q", tail(got, 300))
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

// TestDecisionsListKeepsADecisionLargerThanTheBudget is the tool-level regression
// for the one oversized row: each field is clamped to 8000 separately, so one
// decision record is about 16 KB — larger than the whole response budget — and
// the listing must still return it rather than a notice over nothing.
func TestDecisionsListKeepsADecisionLargerThanTheBudget(t *testing.T) {
	_, session := newCapSession(t)
	decision := strings.Repeat("d", 7900)
	rationale := strings.Repeat("r", 7900)
	res := callTool(t, session, "ghost_decision_record", map[string]any{
		"project_id": "test-project",
		"title":      "huge decision",
		"decision":   decision,
		"rationale":  rationale,
	})
	if res.IsError {
		t.Fatalf("decision_record: %s", resultText(res))
	}

	out := resultText(callTool(t, session, "ghost_decisions_list", map[string]any{
		"project_id": "test-project",
		"limit":      1,
	}))
	if !strings.Contains(out, "huge decision") || !strings.Contains(out, decision) {
		t.Fatalf("the oversized decision was dropped, so the listing cannot read it back: bytes=%d:\n%s",
			len(out), tail(out, 400))
	}
	if !strings.Contains(out, "shown in full") {
		t.Errorf("the notice does not say the oversized decision was kept whole: %q", tail(out, 400))
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

// TestMemorySearchWithNoMatchStillAbstains is the control for the rule above:
// keeping the first row is about a row that exists, so a search that finds
// nothing still answers empty with its own reason (with no embedder that reason
// is the vector leg's, as it was before).
func TestMemorySearchWithNoMatchStillAbstains(t *testing.T) {
	_, session := newCapSession(t)
	saveMemory(t, session, "an unrelated memory about giraffes")
	out := resultText(callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project",
		"query":      "zebra",
	}))
	if !strings.Contains(out, "[ghost:outcome=empty reason=vector_backend_unavailable ") ||
		!strings.Contains(out, " candidates=0 admitted=0 ") {
		t.Errorf("a search that matched nothing does not abstain as it did:\n%s", out)
	}
	if strings.Contains(out, "shown in full") {
		t.Errorf("an answer with no row carries the first-row notice:\n%s", out)
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
