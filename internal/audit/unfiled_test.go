package audit

import (
	"os"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestTheSummaryCountsOnlyTheVerdictsThatWereFiled is the other half of the
// store's report: a refusal the summary does not subtract is a figure describing a
// table it does not match.
//
// The store refuses a verdict whose call was purged after the run read it, and it
// returns exactly which ones. Without dropUnfiled, Run counts every verdict as it
// judges it — into res.Verdicts, into VerdictList, and into the per-source buckets
// — and then writes them, and a refusal vanishes inside the write. The printed line
// would read "3 verdict(s)" over a table holding two, with no record anywhere that
// the third was lost: a hole nobody can see is not an honest hole, it is a wrong
// number, and to the one reader who has to act on it it is indistinguishable from
// a clean run. Every figure the report prints has to describe what the table HOLDS.
//
// A real purge landing inside Run's read-judge-write window is a race, and a test
// that arranges one is a flaky test. So this tests the reconciliation directly,
// over the same two lists Run hands it.
func TestTheSummaryCountsOnlyTheVerdictsThatWereFiled(t *testing.T) {
	kept := []placed{
		{verdict: Verdict{MemoryID: "MEM-1", Outcome: OutcomeUsed, Signal: SignalIdentifier},
			record: 1, source: "search"},
		{verdict: Verdict{MemoryID: "MEM-2", Outcome: OutcomeIgnored},
			record: 1, source: "search"},
		{verdict: Verdict{MemoryID: "MEM-3", Outcome: OutcomeContradicted, Signal: SignalNegation},
			record: 2, source: "session_start"},
	}
	// Seeded EMPTY and counted below, the way Run does it: writing the figures in
	// by hand and then counting them again would double every bucket and the test
	// would be asserting against its own arithmetic.
	bySource := map[string]*SourceSummary{
		"search":        {Source: "search", Calls: 1},
		"session_start": {Source: "session_start", Calls: 1},
	}
	// What Run counts before it writes, which is what it would print unchanged.
	res := Summary{ProjectID: "p1", Verdicts: len(kept)}
	for _, p := range kept {
		res.VerdictList = append(res.VerdictList, p.verdict)
		bySource[p.source].count(p.verdict.Outcome)
	}
	for _, name := range []string{"search", "session_start"} {
		res.Sources = append(res.Sources, *bySource[name])
	}

	// The store refused the middle row: its call was purged after the run read it.
	refused := []memory.RetrievalAuditRow{
		{ProjectID: "p1", RecordRowID: 1, Source: "search", MemoryID: "MEM-2", Outcome: string(OutcomeIgnored)},
	}
	res.dropUnfiled(kept, bySource, refused)
	for i := range res.Sources {
		if sum := bySource[res.Sources[i].Source]; sum != nil {
			res.Sources[i] = *sum
		}
	}

	if res.Verdicts != 2 {
		t.Errorf("Verdicts = %d, want 2 — the refused verdict is not in the table, so a report of 3 describes a "+
			"table it does not match", res.Verdicts)
	}
	if len(res.VerdictList) != 2 {
		t.Fatalf("VerdictList holds %d verdict(s), want 2: %+v", len(res.VerdictList), res.VerdictList)
	}
	for _, v := range res.VerdictList {
		if v.MemoryID == "MEM-2" {
			t.Error("VerdictList still holds MEM-2, which was never filed — a caller reading the list counts it")
		}
	}
	// Order is the order the calls were read, and a refusal in the middle must not
	// reorder the survivors past it.
	if res.VerdictList[0].MemoryID != "MEM-1" || res.VerdictList[1].MemoryID != "MEM-3" {
		t.Errorf("VerdictList is %q, %q — want MEM-1, MEM-3 in the order the calls were read",
			res.VerdictList[0].MemoryID, res.VerdictList[1].MemoryID)
	}
	if got := res.Sources[0].Ignored; got != 0 {
		t.Errorf("the search source still counts %d ignored, want 0 — the refused verdict was an ignored one "+
			"under search, so the printed per-source line is wrong by the same amount as the total", got)
	}
	if got := res.Sources[0].Used; got != 1 {
		t.Errorf("the search source counts %d used, want 1 — only the refused verdict comes out, never its "+
			"neighbours", got)
	}
	if got := res.Sources[1].Contradicted; got != 1 {
		t.Errorf("the session_start source counts %d contradicted, want 1 — a refusal under one source must not "+
			"reach another's bucket", got)
	}
	if res.Unfiled != 1 {
		t.Errorf("Unfiled = %d, want 1 — without it the total is smaller than the run judged and nothing says why",
			res.Unfiled)
	}
}

// TestTheSummaryKeepsEveryFigureWhenNothingWasRefused: the reconciliation is a
// no-op on a clean run, and it has to be, because the clean run is the common one
// and a correction that fires on it would make every report wrong.
func TestTheSummaryKeepsEveryFigureWhenNothingWasRefused(t *testing.T) {
	kept := []placed{
		{verdict: Verdict{MemoryID: "MEM-1", Outcome: OutcomeUsed}, record: 1, source: "search"},
		{verdict: Verdict{MemoryID: "MEM-2", Outcome: OutcomeIgnored}, record: 1, source: "search"},
	}
	bySource := map[string]*SourceSummary{"search": {Source: "search", Used: 1, Ignored: 1}}
	res := Summary{ProjectID: "p1", Verdicts: 2, VerdictList: []Verdict{kept[0].verdict, kept[1].verdict}}

	res.dropUnfiled(kept, bySource, nil)

	if res.Verdicts != 2 || len(res.VerdictList) != 2 {
		t.Errorf("a clean run reports %d verdict(s) over a list of %d, want 2 and 2 — the reconciliation must not "+
			"fire when nothing was refused", res.Verdicts, len(res.VerdictList))
	}
	if bySource["search"].Used != 1 || bySource["search"].Ignored != 1 {
		t.Errorf("the per-source figures moved on a clean run: %+v", *bySource["search"])
	}
	if res.Unfiled != 0 {
		t.Errorf("Unfiled = %d on a clean run, want 0", res.Unfiled)
	}
}

// TestRunReconcilesWhatTheStoreRefused pins the CALL SITE, because the tests above
// only prove the reconciliation is correct — not that Run performs it. Deleting the
// one line in Run that invokes it leaves every other test in this file green, which
// is the shape of the bug this whole change is about: correct code, not called.
//
// So this reads Run's body. It is a source scan rather than a behavioural test
// because the condition it guards is a RACE — a purge landing between Run's read
// and its write — and a test that arranges one is a flaky test, which is worse than
// no test. What is asserted is that the store's return value is CAPTURED (not
// discarded) and reaches the reconciliation with both lists it needs: `kept`, which
// carries each verdict's source and outcome, and `bySource`, which is the map the
// printed per-source lines are built from afterwards. A scan cannot see whether the
// arithmetic is right — the tests above do that — and it does not need to.
func TestRunReconcilesWhatTheStoreRefused(t *testing.T) {
	src, err := os.ReadFile("run.go")
	if err != nil {
		t.Fatalf("read run.go: %v", err)
	}
	body := string(src)
	i := strings.Index(body, "func Run(ctx context.Context")
	if i < 0 {
		t.Fatal("run.go no longer has a Run function at the top level")
	}
	body = body[i:]
	if j := strings.Index(body[1:], "\nfunc "); j >= 0 {
		body = body[:j+1]
	}

	if !strings.Contains(body, "refused, err := store.RecordRetrievalAudits") &&
		!strings.Contains(body, "refused, err = store.RecordRetrievalAudits") {
		t.Error("Run does not capture what RecordRetrievalAudits returned — the refusals are discarded, so the " +
			"figures it prints describe a table the refusals are not in")
	}
	if !strings.Contains(body, "dropUnfiled(kept, bySource, refused)") {
		t.Error("Run does not pass the refused rows to the reconciliation — it counted every verdict it judged " +
			"and never takes the refused ones back out, so the report claims a number the table does not hold")
	}
	// The per-source lines are rendered from res.Sources, which Run fills from
	// bySource BEFORE the write. Without a refresh afterwards the correction reaches
	// the total and not the breakdown, which is the same wrong number one level
	// down.
	if !strings.Contains(body, "res.Sources[i] = *sum") {
		t.Error("Run does not refresh res.Sources from the corrected bySource after the write — the per-source " +
			"lines would still count verdicts that were never stored")
	}
	if !strings.Contains(body, "if len(rows) > 0 {") {
		t.Error("Run no longer guards the write on a non-empty batch; if it calls the store with nothing, the " +
			"reconciliation is skipped and Unfiled stays 0 for a run that filed nothing at all")
	}
}

// TestTheSummaryNamesTheRefusals: a figure that silently excludes the refused
// verdicts is its own wrong number — a reader cannot tell a clean run from a lossy
// one, and the lossy one is the one they need to know about. So the count is
// printed, with the reason, and printed ONLY when there is one.
func TestTheSummaryNamesTheRefusals(t *testing.T) {
	printed := Summary{ProjectID: "p1", Verdicts: 2, Unfiled: 1}.String()
	if !strings.Contains(printed, "1 verdict(s) were not filed") {
		t.Errorf("the summary does not name the refused verdict:\n%s", printed)
	}
	if !strings.Contains(printed, "purged") {
		t.Errorf("the summary names the refusal without saying why, so a reader cannot tell a pair lost to a "+
			"purge from a write that failed:\n%s", printed)
	}
	// It also has to reconcile the two numbers, or the reader is left to notice the
	// shortfall themselves.
	if !strings.Contains(printed, "2 stored") || !strings.Contains(printed, "3 judged") {
		t.Errorf("the summary does not state both figures, so the reader cannot see that 2 of 3 were stored:\n%s",
			printed)
	}

	// And absent when there is nothing to report: a line that always prints is a
	// line an operator learns to skip, which is the state this whole change exists
	// to leave impossible.
	clean := Summary{ProjectID: "p1", Verdicts: 2}.String()
	if strings.Contains(clean, "not filed") {
		t.Errorf("a clean run prints a refusal line it has nothing to report:\n%s", clean)
	}
}
