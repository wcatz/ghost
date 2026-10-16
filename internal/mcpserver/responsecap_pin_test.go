package mcpserver

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The project-context byte cut and a pin. A pinned memory is guaranteed a slot
// on ghost_project_context ahead of ranking, so the response byte budget may cut
// a pinned row only once every unpinned row is gone, says how many of the rows it
// cut were pinned, and never leaves a section heading over nothing.

// nearCapContent is one row of about 7,900 bytes, unique per index so the save
// path's near-duplicate folding keeps every row: two of them with their framing
// are just under the 16,000-byte budget, three are over it.
func nearCapContent(i int) string {
	return fmt.Sprintf("bulk %02d ", i) + strings.Repeat(fmt.Sprintf("w%02d ", i), 1970)
}

// savePinned saves content through the named save tool, pins it in pinProject
// and returns its id.
func savePinned(t *testing.T, session *mcp.ClientSession, tool, pinProject, content string) string {
	t.Helper()
	args := map[string]any{"content": content, "category": "preference"}
	if tool == "ghost_memory_save" {
		args["project_id"] = "test-project"
	}
	res := callTool(t, session, tool, args)
	if res.IsError {
		t.Fatalf("%s: %s", tool, resultText(res))
	}
	id, ok := extractID(resultText(res))
	if !ok {
		t.Fatalf("%s response carries no id: %q", tool, resultText(res))
	}
	pin := callTool(t, session, "ghost_memory_pin", map[string]any{
		"project_id": pinProject,
		"memory_id":  id,
		"pinned":     true,
	})
	if pin.IsError {
		t.Fatalf("pin %s: %s", id, resultText(pin))
	}
	return id
}

func projectContextOut(t *testing.T, session *mcp.ClientSession) string {
	t.Helper()
	res := callTool(t, session, "ghost_project_context", map[string]any{
		"project_id": "test-project",
		"limit":      100,
	})
	if res.IsError {
		t.Fatalf("project_context failed: %s", resultText(res))
	}
	return resultText(res)
}

// emptyHeading matches a `## ` heading followed by nothing but blank lines and
// then another heading, a parenthesised notice or the end of the answer.
var emptyHeading = regexp.MustCompile(`(?m)^## [^\n]*\n\n(?:\n|## |\(|$)`)

func assertNoEmptyHeading(t *testing.T, out string) {
	t.Helper()
	if loc := emptyHeading.FindStringIndex(out); loc != nil {
		t.Errorf("a section heading is left over no rows: %q", out[loc[0]:min(loc[1]+80, len(out))])
	}
}

// pinnedCutCount is the number of pinned rows the byte cut reports, or -1 when
// the answer reports none.
var pinnedByteCut = regexp.MustCompile(`\((\d+) of the cut rows (?:was|were) pinned`)

func pinnedCutCount(out string) int {
	m := pinnedByteCut.FindStringSubmatch(out)
	if m == nil {
		return -1
	}
	n := 0
	_, _ = fmt.Sscanf(m[1], "%d", &n)
	return n
}

// TestProjectContextByteCutKeepsAPinnedGlobalRow is the reported shape: six
// near-cap project rows and one small pinned global preference. The cut used to
// remove the global row with the project rows below the first, and leave the
// `## Global` heading over nothing.
func TestProjectContextByteCutKeepsAPinnedGlobalRow(t *testing.T) {
	_, session := newCapSession(t)
	const n = 6
	for i := range n {
		saveMemory(t, session, nearCapContent(i))
	}
	pref := "always sign commits and never force-push to the default branch " + strings.Repeat("p", 180)
	savePinned(t, session, "ghost_save_global", "_global", pref)

	out := projectContextOut(t, session)
	assertWithinBudget(t, out)
	if !strings.Contains(out, pref) {
		t.Errorf("the pinned global row was cut although it fits: bytes=%d\n%s", len(out), tail(out, 600))
	}
	if !strings.Contains(out, globalSectionHeading+"\n\n- [") {
		t.Errorf("the Global section does not carry its pinned row: %s", tail(out, 600))
	}
	assertNoEmptyHeading(t, out)
	kept := countItemLines(out)
	cut := cutCount(t, out)
	if cut < 1 {
		t.Fatalf("no cut was reported, so the fixture did not exceed the cap: bytes=%d", len(out))
	}
	if kept+cut != n+1 {
		t.Errorf("%d rows kept + %d cut = %d, want %d", kept, cut, kept+cut, n+1)
	}
	if p := pinnedCutCount(out); p != -1 {
		t.Errorf("no pinned row was cut, but the answer reports %d: %s", p, tail(out, 400))
	}
	// The kept pinned row renders AFTER the cut rows, so "from the end" would be
	// false: the notice says unpinned rows went first instead.
	if strings.Contains(out, "cut from the end of this answer") {
		t.Errorf("the notice says the rows were cut from the end, but a kept row follows them: %s", tail(out, 400))
	}
	if !strings.Contains(out, fmt.Sprintf("%d rows were cut from this answer, unpinned rows first, to stay within the 16000-byte response budget", cut)) {
		t.Errorf("the notice does not say unpinned rows were cut first: %s", tail(out, 400))
	}
	if !strings.Contains(out, projectContextCutAdvice) {
		t.Errorf("the pin-aware notice lost the advice: %s", tail(out, 400))
	}
}

// TestProjectContextByteCutKeepsAPinnedProjectRow: a pinned project row is kept
// by the byte cut while unpinned rows are cut, wherever the window ranks it.
func TestProjectContextByteCutKeepsAPinnedProjectRow(t *testing.T) {
	_, session := newCapSession(t)
	const n = 6
	for i := range n {
		saveMemory(t, session, nearCapContent(i))
	}
	pinned := "the deploy runbook lives under docs/ops and is the only source " + strings.Repeat("q", 120)
	savePinned(t, session, "ghost_memory_save", "test-project", pinned)

	out := projectContextOut(t, session)
	assertWithinBudget(t, out)
	if !strings.Contains(out, pinned) {
		t.Errorf("the pinned project row was cut although it fits: bytes=%d\n%s", len(out), tail(out, 600))
	}
	assertNoEmptyHeading(t, out)
	kept := countItemLines(out)
	cut := cutCount(t, out)
	if cut < 1 {
		t.Fatalf("no cut was reported: bytes=%d", len(out))
	}
	if kept+cut != n+1 {
		t.Errorf("%d rows kept + %d cut = %d, want %d", kept, cut, kept+cut, n+1)
	}
	if p := pinnedCutCount(out); p != -1 {
		t.Errorf("no pinned row was cut, but the answer reports %d", p)
	}
}

// TestProjectContextByteCutReportsPinnedRowsAloneOverBudget: when the pinned rows
// alone exceed the budget, pinned rows are cut too — after every unpinned one —
// and the notice says how many of the cut rows were pinned.
func TestProjectContextByteCutReportsPinnedRowsAloneOverBudget(t *testing.T) {
	_, session := newCapSession(t)
	saveMemory(t, session, "a small unpinned fact about the build cache")
	const pins = 4
	for i := range pins {
		savePinned(t, session, "ghost_memory_save", "test-project", nearCapContent(i))
	}

	out := projectContextOut(t, session)
	assertWithinBudget(t, out)
	assertNoEmptyHeading(t, out)
	if strings.Contains(out, "small unpinned fact") {
		t.Errorf("an unpinned row survived while pinned rows were cut: %s", tail(out, 600))
	}
	kept := countItemLines(out)
	cut := cutCount(t, out)
	if kept < 1 {
		t.Fatalf("every row was cut: %s", tail(out, 400))
	}
	if kept+cut != pins+1 {
		t.Errorf("%d rows kept + %d cut = %d, want %d", kept, cut, kept+cut, pins+1)
	}
	if got, want := pinnedCutCount(out), pins-kept; got != want {
		t.Errorf("the answer reports %d pinned rows cut, want %d (kept %d of %d pinned): %s",
			got, want, kept, pins, tail(out, 600))
	}
}

// TestProjectContextByteCutWithoutAPinIsUnchanged is the control: with no pinned
// row, unpinned rows still go from the END, the notice is the plain one, and the
// first row survives.
func TestProjectContextByteCutWithoutAPinIsUnchanged(t *testing.T) {
	_, session := newCapSession(t)
	const n = 6
	for i := range n {
		saveMemory(t, session, nearCapContent(i))
	}
	out := projectContextOut(t, session)
	assertWithinBudget(t, out)
	if strings.Contains(out, globalSectionHeading) {
		t.Errorf("no global row exists, so no Global heading may print: %s", tail(out, 400))
	}
	if cutCount(t, out) != n-countItemLines(out) || pinnedCutCount(out) != -1 {
		t.Errorf("the plain cut notice is wrong: %s", tail(out, 400))
	}
	if !strings.Contains(out, "cut from the end of this answer") {
		t.Errorf("a pin-free cut must keep the end-first wording: %s", tail(out, 400))
	}
	if !strings.HasPrefix(out, memorySectionHeading+"\n\n- [") {
		t.Errorf("the answer must open with the Memories section and its first row: %q", out[:min(200, len(out))])
	}
}

// TestCapResponsePartsCutsUnpinnedRowsBeforeAPinnedOne: a pinned row ranked
// BELOW unpinned rows in its own section — a demotion reorder can place it there
// — survives the cut while the unpinned rows below and above it go.
func TestCapResponsePartsCutsUnpinnedRowsBeforeAPinnedOne(t *testing.T) {
	r0 := "- r0 " + strings.Repeat("a", 400) + "\n"
	r1 := "- r1 " + strings.Repeat("b", 400) + "\n"
	pin := "- pinned row\n"
	r3 := "- r3 " + strings.Repeat("c", 400) + "\n"
	parts := responseSection(0, memorySectionHeading, []string{r0, r1, pin, r3}, []bool{false, false, true, false}, "")
	budget := 900
	got, cut := capResponseParts(parts, budget, "Advise.")
	if len(got) > budget {
		t.Fatalf("the answer is %d bytes against a %d-byte budget", len(got), budget)
	}
	if !strings.Contains(got, pin) {
		t.Errorf("the pinned row was cut while unpinned rows were left to cut: %q", got)
	}
	if !strings.Contains(got, r0) || strings.Contains(got, r1) || strings.Contains(got, r3) {
		t.Errorf("unpinned rows must go from the end: want r0 kept, r1 and r3 cut: %q", got)
	}
	if cut != 2 || pinnedCutCount(got) != -1 {
		t.Errorf("cut = %d pinned = %d, want 2 cut and no pinned-cut statement: %q", cut, pinnedCutCount(got), got)
	}
}

// TestCapResponsePartsDropsAnEmptiedHeading: the `## Global` section's only row
// is unpinned and the cut removes it, so its heading goes too and the notice
// stays.
func TestCapResponsePartsDropsAnEmptiedHeading(t *testing.T) {
	r0 := "- r0 " + strings.Repeat("a", 400) + "\n"
	g0 := "- g0 " + strings.Repeat("g", 400) + "\n"
	parts := responseSection(0, memorySectionHeading, []string{r0}, nil, "")
	parts = append(parts, responseSection(responsePartsLen(parts), globalSectionHeading, []string{g0}, nil, "")...)
	got, cut := capResponseParts(parts, 700, "Advise.")
	if cut != 1 || strings.Contains(got, g0) {
		t.Fatalf("cut = %d, want the global row cut: %q", cut, got)
	}
	if strings.Contains(got, globalSectionHeading) {
		t.Errorf("the emptied Global heading was printed: %q", got)
	}
	// A section that keeps a trailing note keeps its heading: the note is about it.
	parts = responseSection(0, memorySectionHeading, []string{r0}, nil, "")
	parts = append(parts, responseSection(responsePartsLen(parts), globalSectionHeading, []string{g0}, nil, "(a note)\n")...)
	got, _ = capResponseParts(parts, 700, "Advise.")
	if !strings.Contains(got, globalSectionHeading+"\n\n(a note)\n") {
		t.Errorf("a section whose note survives must keep its heading: %q", got)
	}
}

// TestCapResponsePartsOpensWithAKeptLaterSection: when every row of the first
// section is cut and a pinned row in the second survives, the answer opens with
// the second heading and no leading blank lines.
func TestCapResponsePartsOpensWithAKeptLaterSection(t *testing.T) {
	r0 := "- r0 " + strings.Repeat("a", 600) + "\n"
	g0 := "- g0 pinned\n"
	parts := responseSection(0, memorySectionHeading, []string{r0}, nil, "")
	parts = append(parts, responseSection(responsePartsLen(parts), globalSectionHeading, []string{g0}, []bool{true}, "")...)
	got, cut := capResponseParts(parts, 450, "Advise.")
	if cut != 1 || len(got) > 450 {
		t.Fatalf("cut = %d bytes = %d, want the unpinned row cut within 450: %q", cut, len(got), got)
	}
	if !strings.HasPrefix(got, globalSectionHeading+"\n\n"+g0) {
		t.Errorf("the answer must open with the kept Global section: %q", got)
	}
}

// TestCapResponsePartsReportsPinnedRowsAloneOverBudget: three pinned rows that do
// not fit together. Every unpinned row goes first, then pinned rows from the end,
// the first pinned row survives, and the notice counts the pinned ones.
func TestCapResponsePartsReportsPinnedRowsAloneOverBudget(t *testing.T) {
	p0 := "- p0 " + strings.Repeat("a", 600) + "\n"
	p1 := "- p1 " + strings.Repeat("b", 600) + "\n"
	p2 := "- p2 " + strings.Repeat("c", 600) + "\n"
	u0 := "- u0 small\n"
	parts := responseSection(0, memorySectionHeading, []string{p0, p1, p2, u0}, []bool{true, true, true, false}, "")
	budget := 1800
	got, cut := capResponseParts(parts, budget, "Advise.")
	if len(got) > budget {
		t.Fatalf("the answer is %d bytes against a %d-byte budget: %q", len(got), budget, got)
	}
	if !strings.Contains(got, p0) || strings.Contains(got, u0) || strings.Contains(got, p2) {
		t.Errorf("want p0 kept, u0 then p2 cut: %q", got)
	}
	keptPinned := strings.Count(got, "- p")
	if cut != 4-keptPinned || pinnedCutCount(got) != 3-keptPinned {
		t.Errorf("cut = %d pinned reported = %d with %d pinned kept: %q", cut, pinnedCutCount(got), keptPinned, got)
	}
	if !strings.Contains(got, "pinned rows alone exceed the 1800-byte response budget") {
		t.Errorf("the pinned-cut statement does not say why: %q", got)
	}
}

// TestCapResponsePartsWithoutPinsMatchesTheEndCut is the control: with no pinned
// row, the kept rows are a prefix of the rows, exactly as the end-first cut kept
// them, for every budget from the framing up to the full render.
func TestCapResponsePartsWithoutPinsMatchesTheEndCut(t *testing.T) {
	rows := []string{}
	for i := range 6 {
		rows = append(rows, fmt.Sprintf("- r%d %s\n", i, strings.Repeat("x", 100+40*i)))
	}
	full := joinResponseParts(responseSection(0, memorySectionHeading, rows, nil, ""))
	for budget := 50; budget <= len(full)+10; budget += 7 {
		parts := responseSection(0, memorySectionHeading, rows, nil, "")
		got, cut := capResponseParts(parts, budget, "Advise.")
		kept := len(rows) - cut
		prefix := memorySectionHeading + "\n\n" + strings.Join(rows[:kept], "")
		if !strings.HasPrefix(got, prefix) {
			t.Fatalf("budget %d: the kept rows are not the first %d rows: %q", budget, kept, got)
		}
		if kept < len(rows) && strings.Contains(got, rows[kept]) {
			t.Fatalf("budget %d: row %d survived a cut from the end", budget, kept)
		}
		if cut > 0 && kept > 1 && len(got) > budget {
			t.Fatalf("budget %d: the answer is %d bytes", budget, len(got))
		}
		if cut == 0 && got != full {
			t.Fatalf("budget %d: an uncut answer must be byte-identical", budget)
		}
	}
}
