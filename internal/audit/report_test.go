package audit

// #646 part 3: the REPORT. Part 2 wrote verdicts; this reads them back and
// prints per-source figures, and every property below is about what the printed
// bytes are allowed to mean.
//
// Four of them are load-bearing:
//
//  1. Sources are never pooled. A ratio over "used of kept" pooled across a
//     search and an injection is a number about neither question.
//  2. A source with no rows says so, rather than reporting 0% — which would read
//     as "nothing it showed was used" about a source that has never run.
//  3. Precision is used / SCORED, and scored is the verdict count — not the call
//     count (one call can keep twenty memories) and not the kept count either (a
//     kept memory no run has judged yet is not a denominator).
//  4. The output carries ids and counts only. No memory content, no query text,
//     no signal text — the same constraint part 1 and part 2 held to the rows,
//     held to the report.

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// reportStore is a store holding one project, ready for the report fixtures. The
// memories are seeded with distinctive wording so the comparison arms have
// something to match.
//
// It opens the store itself rather than reusing auditStore because the fixtures
// need the DATABASE PATH: recorded_at is stamped by SQLite at write time and no
// store API can backdate it, so a --since fixture has to reach the file. A second
// handle is how the rest of the suite does it (see mcpserver's seedRestatements).
func reportStore(t *testing.T) (*memory.Store, string, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	store := memory.NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := store.EnsureProject(ctx, "p1", "/tmp/audit-report-p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	seedMemory(t, store, "p1", "USEDID", memContent)
	seedMemory(t, store, "p1", "IGNID", "Bench seeds restore content through the shared clamp helper")
	seedMemory(t, store, "p1", "CONID", "The v20 migration runs before the pre-migration backup")
	return store, "p1", dbPath
}

// backdateRetrievalRows moves both tables' recorded_at for a project into the
// past, through a SECOND handle on the file.
//
// Both tables, because a window that filtered one and not the other would report
// calls with nothing kept by them, and that is exactly the bug
// TestReportSinceFiltersBothTables exists to fail.
func backdateRetrievalRows(t *testing.T, dbPath, projectID string, ago time.Duration) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open %s to backdate: %v", dbPath, err)
	}
	defer db.Close() //nolint:errcheck
	stamp := time.Now().Add(-ago).UTC().Format(memory.StoredStampLayout)
	for _, table := range []string{"retrieval_record", "retrieval_audit"} {
		if _, err := db.Exec(`UPDATE `+table+` SET recorded_at = ? WHERE project_id = ?`, stamp, projectID); err != nil {
			t.Fatalf("backdate %s: %v", table, err)
		}
	}
}

// judge runs one audit over the seeded project so the report has verdicts, which
// is the only way a report fixture reaches the state a real session reaches.
func judge(t *testing.T, store *memory.Store, projectID string, s *Signals) {
	t.Helper()
	if _, err := Run(context.Background(), store, projectID, s); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestReportKeepsEachSourceSeparate is property 1. Two sources, deliberately with
// opposite figures, and the assertion is that NEITHER number moves when the other
// source is present. A pooled implementation passes a test that only checks the
// total, so this one pins the per-source figure itself.
func TestReportKeepsEachSourceSeparate(t *testing.T) {
	store, projectID, _ := reportStore(t)
	seedMemory(t, store, projectID, "SSID", "Scratch directories are reaped before each lifecycle run begins")
	seedMemory(t, store, projectID, "SSIGN", "The relay listens on port 2222 in production")

	// A search that kept three memories: one used, one contradicted, one ignored.
	_ = recordCall(t, store, projectID, "search", "USEDID", "IGNID", "CONID")
	// A session-start injection that kept two, and used neither.
	_ = recordCall(t, store, projectID, "session_start", "SSID", "SSIGN")

	s := newTestSignals(t)
	s.AddProse("the opencode plugin materializes its transcript under mkdtemp")
	s.AddProse("that is wrong: the v20 migration runs after the pre-migration backup")
	judge(t, store, projectID, s)

	rep, err := BuildReport(context.Background(), store, ReportOptions{ProjectID: projectID})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}

	search := rep.Source("search")
	if search == nil {
		t.Fatal("the report has no search figures at all")
	}
	// 3 kept, 1 used. The precision is over KEPT MEMORIES, not over calls: a
	// pooled-or-call-denominator implementation would say 1/1 here.
	if search.Kept != 3 || search.Used != 1 || search.Scored != 3 {
		t.Errorf("search kept %d, scored %d, used %d; want 3/3/1", search.Kept, search.Scored, search.Used)
	}
	if got, want := search.PrecisionPercent(), 33; got != want {
		t.Errorf("search precision = %d%%, want %d%%", got, want)
	}

	start := rep.Source("session_start")
	if start == nil {
		t.Fatal("the report has no session_start figures at all")
	}
	if start.Kept != 2 || start.Used != 0 || start.Scored != 2 {
		t.Errorf("session_start kept %d, scored %d, used %d; want 2/2/0", start.Kept, start.Scored, start.Used)
	}
	if got := start.PrecisionPercent(); got != 0 {
		t.Errorf("session_start precision = %d%%, want 0%% — and a SOURCE with rows may say 0", got)
	}
	// The proof that nothing pooled: search's figure is unaffected by a source
	// sitting at zero, and vice versa.
	if rep.Pooled() != nil {
		t.Errorf("the report exposes a pooled figure (%v); the sources must never be pooled", *rep.Pooled())
	}
}

// TestReportSaysNoRowsRatherThanZeroPercent is property 2, and it is the one a
// fresh install hits: `ghost context --audit` on a store nobody has searched has
// three sources and no rows at all. "0% used" would be a measurement of nothing.
func TestReportSaysNoRowsRatherThanZeroPercent(t *testing.T) {
	store, projectID, _ := reportStore(t)

	rep, err := BuildReport(context.Background(), store, ReportOptions{ProjectID: projectID})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}

	out := rep.String()
	for _, source := range KnownSources {
		if !strings.Contains(out, source+": no rows") {
			t.Errorf("the report does not say %s has no rows:\n%s", source, out)
		}
		if strings.Contains(out, source+": 0%") {
			t.Errorf("the report printed a precision for %s, which has no rows:\n%s", source, out)
		}
	}
	// And every known source is NAMED. A source absent from the report cannot be
	// told apart from a source this Ghost does not know about, and #850's passive
	// sources are exactly the case: before their wiring lands they have no rows,
	// and the report has to say so rather than stay silent about them.
	for _, source := range KnownSources {
		if !strings.Contains(out, source) {
			t.Errorf("the report never names the known source %s:\n%s", source, out)
		}
	}
}

// TestReportNamesSourcesThatHaveNoRowsWhileOthersDo: mixed state is the state a
// real install reaches — a searched project before #850's wiring lands has
// search figures and empty session_start. The empty one must still be named, and
// must not borrow a percentage.
func TestReportNamesSourcesThatHaveNoRowsWhileOthersDo(t *testing.T) {
	store, projectID, _ := reportStore(t)
	_ = recordCall(t, store, projectID, "search", "USEDID")
	s := newTestSignals(t)
	s.AddProse("the opencode plugin materializes its transcript under mkdtemp")
	judge(t, store, projectID, s)

	rep, err := BuildReport(context.Background(), store, ReportOptions{ProjectID: projectID})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}

	out := rep.String()
	if !strings.Contains(out, "search:") {
		t.Errorf("the report lost the source that has rows:\n%s", out)
	}
	for _, source := range []string{"session_start", "project_context"} {
		if !strings.Contains(out, source+": no rows") {
			t.Errorf("the report does not say %s has no rows alongside a source that does:\n%s", source, out)
		}
	}
	if strings.Contains(out, "session_start: 0%") {
		t.Errorf("the report gave an empty source a precision:\n%s", out)
	}
}

// TestReportListsContradictedIDsAndNothingElse is property 4, asserted on the
// printed bytes rather than on a type — because a renderer is where text can
// reach a report even when every field it reads is an id or a count.
//
// The seeded wording is deliberately distinctive, and the query the call recorded
// a digest of is never printed either: the report has no access to the query text
// and must not acquire one.
func TestReportListsContradictedIDsAndNothingElse(t *testing.T) {
	store, projectID, _ := reportStore(t)
	_ = recordCall(t, store, projectID, "search", "USEDID", "IGNID", "CONID")

	s := newTestSignals(t)
	s.AddProse("the opencode plugin materializes its transcript under mkdtemp")
	s.AddProse("that is wrong: the v20 migration runs after the pre-migration backup")
	judge(t, store, projectID, s)

	rep, err := BuildReport(context.Background(), store, ReportOptions{ProjectID: projectID})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	out := rep.String()

	if !strings.Contains(out, "CONID") {
		t.Errorf("the report does not name the contradicted memory:\n%s", out)
	}
	// The content the comparison read, and the agent's own prose, must not be
	// reachable from the report.
	for _, leak := range []string{
		"v20 migration runs before the pre-migration backup",
		"migration runs before",
		"materializes its transcript",
		"that is wrong",
	} {
		if strings.Contains(out, leak) {
			t.Errorf("the report leaked %q into its output:\n%s", leak, out)
		}
	}
	// And only the CONTRADICTED one is listed: an id list that named every
	// verdict would answer a different question, and would put the corpus's ids
	// in a report an operator pastes into an issue.
	if strings.Contains(out, "IGNID") || strings.Contains(out, "USEDID") {
		t.Errorf("the report lists ids that were not contradicted:\n%s", out)
	}
}

// TestReportCountsDegradedVerdictsAndNamesTheReason: a verdict filed under a
// partial transcript read is a claim about the text that WAS read. A report that
// counted those verdicts into a clean precision would be overstating what it
// knows, and one that dropped them would be silently reporting a smaller
// denominator than the store holds.
func TestReportCountsDegradedVerdictsAndNamesTheReason(t *testing.T) {
	store, projectID, _ := reportStore(t)
	_ = recordCall(t, store, projectID, "search", "USEDID", "IGNID")

	// A degraded run: the scanner says it only read part of the transcript.
	degradedSignals := newTestSignals(t)
	degradedSignals.MarkDegraded("transcript truncated")
	degradedSignals.AddProse("the opencode plugin materializes its transcript under mkdtemp")
	judge(t, store, projectID, degradedSignals)

	rep, err := BuildReport(context.Background(), store, ReportOptions{ProjectID: projectID})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}

	search := rep.Source("search")
	if search == nil {
		t.Fatal("the report has no search figures")
	}
	if search.DegradedVerdicts != 2 {
		t.Errorf("DegradedVerdicts = %d, want 2 — both verdicts were filed under a partial read", search.DegradedVerdicts)
	}
	if search.Scored != 2 {
		t.Errorf("Scored = %d, want 2 — a degraded verdict is still a verdict and still in the denominator", search.Scored)
	}
	out := rep.String()
	if !strings.Contains(out, "degraded") || !strings.Contains(out, "transcript truncated") {
		t.Errorf("the report does not name the degraded verdict count and its reason:\n%s", out)
	}
}

// TestReportStatesItsLimitsOnItsFace: "ignored" is the most misreadable number
// this audit produces, and the owner's own note on the issue is that the report
// has to say what it is not. These are the same sentences part 2's summary
// prints, asserted on the report because a report is a different artifact read by
// a different person.
func TestReportStatesItsLimitsOnItsFace(t *testing.T) {
	store, projectID, _ := reportStore(t)
	_ = recordCall(t, store, projectID, "search", "USEDID", "IGNID")
	s := newTestSignals(t)
	s.AddProse("the opencode plugin materializes its transcript under mkdtemp")
	judge(t, store, projectID, s)

	rep, err := BuildReport(context.Background(), store, ReportOptions{ProjectID: projectID})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	out := rep.String()

	for _, want := range []string{
		// "ignored" is a statement about the transcript, not about the memory.
		"not a relevance or usefulness score",
		// Sources are separate denominators.
		"never pooled",
		// The undetectable half of "missed" must read as NOT MEASURED. Printing
		// it as 0 would be the worst possible lie: a zero here reads as "nothing
		// was re-derived", which is the one claim no heuristic here can support.
		"re-derived",
		"not measured",
		// The detectable half IS reported, and the report says so.
		"kept nothing",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not state %q:\n%s", want, out)
		}
	}
}

// TestReportKeptNothingCountsOnlySearchesThatAdmittedNothing: the detectable
// half of "missed". A call whose kept set is empty is a lookup the agent made
// that returned nothing, and it is only a MISS if the source is one where the
// agent chose to look — an injection that admitted nothing is not a failed
// search.
//
// The report counts both but distinguishes them by name, because pooling them
// would report an injection's silence as a search failure.
func TestReportKeptNothingCountsOnlySearchesThatAdmittedNothing(t *testing.T) {
	store, projectID, _ := reportStore(t)
	// A search that kept nothing, and a search that kept one.
	_ = recordCall(t, store, projectID, "search")
	_ = recordCall(t, store, projectID, "search", "IGNID")
	// An injection that kept nothing.
	_ = recordCall(t, store, projectID, "session_start")

	// No judging: kept-nothing is a property of the RECORD, read straight off the
	// call, and a report must report it even in a store nobody has audited yet.
	rep, err := BuildReport(context.Background(), store, ReportOptions{ProjectID: projectID})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}

	search := rep.Source("search")
	if search == nil {
		t.Fatal("the report has no search figures")
	}
	if search.KeptNothing != 1 {
		t.Errorf("search KeptNothing = %d, want 1 — one of the two searches admitted nothing", search.KeptNothing)
	}
	// Kept is read off the RECORD, not off the verdicts, which is why it is
	// available with no audit run at all — the whole point of a report over a
	// store nobody has judged yet.
	if search.Kept != 1 {
		t.Errorf("search Kept = %d, want 1", search.Kept)
	}
	if search.Scored != 0 {
		t.Errorf("search Scored = %d, want 0 — nothing judged these calls", search.Scored)
	}
	if _, ok := search.Precision(); ok {
		t.Error("search reports a precision over calls no verdict was filed against")
	}
	if !strings.Contains(rep.String(), "no verdict recorded yet") {
		t.Errorf("the report prints a figure for an unaudited source instead of saying so:\n%s", rep.String())
	}
	start := rep.Source("session_start")
	if start == nil {
		t.Fatal("the report has no session_start figures")
	}
	if start.KeptNothing != 1 {
		t.Errorf("session_start KeptNothing = %d, want 1 — an injection that admitted nothing is still counted, and named as such", start.KeptNothing)
	}
	if search.Calls != 2 {
		t.Errorf("search Calls = %d, want 2", search.Calls)
	}
}

// TestTheKeptNothingCountIsNamedAfterTheSourceThatRanIt: the test above proves the
// two figures are counted separately; this proves the reader can TELL them apart,
// which is the half that was missing.
//
// Both renderers print one line per source and both are followed by a sentence
// saying that the detectable half of "missed" is what SEARCHES kept nothing. A line
// that named every source's figure "kept nothing" therefore put
// `session_start: 40 call(s), 2 kept, ..., 30 kept nothing` directly under it, so a
// reader (or an agent reading the health block) tallying missed retrievals adds an
// injection's silence — which is the normal state of a healthy session start — to a
// count of failed lookups. The count is the same; the NAME is what carries which
// question it answers, so the name is per source: a search's is "kept nothing"
// because the agent chose to look and found nothing, an injection's is "admitted
// nothing" because Ghost offered something and the fit stage took none of it.
func TestTheKeptNothingCountIsNamedAfterTheSourceThatRanIt(t *testing.T) {
	store, projectID, _ := reportStore(t)
	// A search that kept nothing, and an injection that admitted nothing: one of
	// each, so both figures are present in the same report and each line has to
	// carry its own name.
	_ = recordCall(t, store, projectID, "search")
	_ = recordCall(t, store, projectID, "session_start")

	rep, err := BuildReport(context.Background(), store, ReportOptions{ProjectID: projectID})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}

	// Summary() is the one line per source a health check carries, built the way
	// health_retrieval.go builds it, so the assertion is over the shape an AGENT
	// reads rather than over a string this test composed for its own convenience.
	var compact strings.Builder
	for _, src := range rep.Sources {
		fmt.Fprintf(&compact, "  %s\n", src.Summary())
	}
	for _, tc := range []struct {
		shape string
		out   string
	}{
		// String() is the human report; the compact form is the health block.
		// Both were named the same way, and a health block is the surface an AGENT
		// reads, so both are asserted.
		{shape: "String()", out: rep.String()},
		{shape: "the health block's one line per source", out: compact.String()},
	} {
		t.Run(tc.shape, func(t *testing.T) {
			searchLine := sourceLine(t, tc.out, "search")
			if !strings.Contains(searchLine, "1 kept nothing") {
				t.Errorf("the search line does not say it kept nothing:\n%s", searchLine)
			}
			if strings.Contains(searchLine, "admitted nothing") {
				t.Errorf("the search line calls a failed lookup an admission:\n%s", searchLine)
			}
			startLine := sourceLine(t, tc.out, "session_start")
			if !strings.Contains(startLine, "1 admitted nothing") {
				t.Errorf("the injection line does not say it admitted nothing:\n%s", startLine)
			}
			if strings.Contains(startLine, "kept nothing") {
				t.Errorf("the injection line names its figure as a failed lookup:\n%s", startLine)
			}
		})
	}
}

// sourceLine is the one line of out that belongs to source, or a failure: a renderer
// that dropped the source would otherwise let both assertions above pass vacuously
// against whatever line happened to be there.
func sourceLine(t *testing.T, out, source string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, source+":") {
			return line
		}
	}
	t.Fatalf("%s prints no line for source %q:\n%s", source, source, out)
	return ""
}

// TestReportSinceFiltersBothTables: --since has only recorded_at to work with on
// both tables, and the rows it excludes must disappear from BOTH halves of the
// figure — a window that filtered the verdicts but not the calls would report
// calls with nothing kept by them.
func TestReportSinceFiltersBothTables(t *testing.T) {
	store, projectID, dbPath := reportStore(t)
	_ = recordCall(t, store, projectID, "search", "USEDID")
	s := newTestSignals(t)
	s.AddProse("the opencode plugin materializes its transcript under mkdtemp")
	judge(t, store, projectID, s)

	backdateRetrievalRows(t, dbPath, projectID, 72*time.Hour)

	// A window that excludes the rows. Every known source is still NAMED — the
	// report says which sources have no rows in this window, rather than going
	// quiet about the ones this window excluded.
	recent, err := BuildReport(context.Background(), store, ReportOptions{ProjectID: projectID, Since: time.Hour})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if src := recent.Source("search"); src == nil {
		t.Error("a 1h window over 72h-old rows does not name the search source at all")
	} else if !src.NoRows() || src.Calls != 0 || src.Kept != 0 {
		t.Errorf("a 1h window over 72h-old rows reports figures: %+v", *src)
	}

	// A window that includes them.
	wide, err := BuildReport(context.Background(), store, ReportOptions{ProjectID: projectID, Since: 30 * 24 * time.Hour})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	src := wide.Source("search")
	if src == nil {
		t.Fatal("a 30d window over 72h-old rows reports no search figures")
	}
	if src.Calls != 1 || src.Kept != 1 || src.Used != 1 {
		t.Errorf("a 30d window reports calls=%d kept=%d used=%d; want 1/1/1", src.Calls, src.Kept, src.Used)
	}
}

// TestReportEchoesItsWindow: a saved report has to say what it measured. A
// figure printed without its window reads as the store's standing state.
func TestReportEchoesItsWindow(t *testing.T) {
	store, projectID, _ := reportStore(t)

	rep, err := BuildReport(context.Background(), store, ReportOptions{ProjectID: projectID, Since: 24 * time.Hour})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if !strings.Contains(rep.String(), "24h") {
		t.Errorf("the report does not echo the window it measured:\n%s", rep.String())
	}
}

// TestReportReadsAnotherProjectsRowsForNothing: the scoping refusal, asserted on
// the figures. Two projects with opposite numbers, and the report is handed one
// of them.
func TestReportReadsAnotherProjectsRowsForNothing(t *testing.T) {
	store, projectID, _ := reportStore(t)
	_ = recordCall(t, store, projectID, "search", "USEDID")

	other := "p2"
	if err := store.EnsureProject(context.Background(), other, "/tmp/audit-report-p2", "p2"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	seedMemory(t, store, other, "OTHERID", "The relay listens on port 2222 in production")
	_ = recordCall(t, store, other, "search", "OTHERID")

	s := newTestSignals(t)
	s.AddProse("the opencode plugin materializes its transcript under mkdtemp")
	judge(t, store, projectID, s)
	judge(t, store, other, s)

	rep, err := BuildReport(context.Background(), store, ReportOptions{ProjectID: projectID})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	src := rep.Source("search")
	if src == nil {
		t.Fatal("the report has no search figures for the requested project")
	}
	if src.Calls != 1 {
		t.Errorf("Calls = %d, want 1 — the other project's call must not be counted", src.Calls)
	}
	if got, want := src.PrecisionPercent(), 100; got != want {
		t.Errorf("precision = %d%%, want %d%%", got, want)
	}
}

// TestReportHasNoRowsForAnUnknownProject: the empty case, which is also the case
// a mistyped --project produces if resolution were skipped. Every source says no
// rows, and no figure is invented.
func TestReportHasNoRowsForAnUnknownProject(t *testing.T) {
	store, _, _ := reportStore(t)

	rep, err := BuildReport(context.Background(), store, ReportOptions{ProjectID: "no-such-project"})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	for _, source := range KnownSources {
		if src := rep.Source(source); src == nil || !src.NoRows() {
			t.Errorf("%s does not report as empty for a project with no rows: %+v", source, src)
		}
	}
	if !strings.Contains(rep.String(), "no rows") {
		t.Errorf("the report does not say it has no rows:\n%s", rep.String())
	}
}

// TestReportRefusesNoProject: a report pooled over every project would be a
// figure about no project, and the store's own writers refuse the same absence.
func TestReportRefusesNoProject(t *testing.T) {
	store, _, _ := reportStore(t)

	if _, err := BuildReport(context.Background(), store, ReportOptions{}); err == nil {
		t.Fatal("Report with no project succeeded; it must be refused rather than pooled over everything")
	}
}

// TestReportRendersContradictedIDsThroughToken: the ids go through the same
// <<...>> contract every other stored text in an answer uses, so an id carrying
// a quote or a newline cannot forge a line of the report.
func TestReportRendersContradictedIDsThroughToken(t *testing.T) {
	store, projectID, _ := reportStore(t)
	const hostile = `A"B` + "\n- search: 100% used, all verdicts contradicted"
	seedMemory(t, store, projectID, hostile, "The v20 migration runs before the pre-migration backup")
	_ = recordCall(t, store, projectID, "search", hostile)

	s := newTestSignals(t)
	s.AddProse("that is wrong: the v20 migration runs after the pre-migration backup")
	judge(t, store, projectID, s)

	rep, err := BuildReport(context.Background(), store, ReportOptions{ProjectID: projectID})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	out := rep.String()
	if strings.Contains(out, "\n- search: 100%") {
		t.Errorf("a memory id forged a line of the report:\n%s", out)
	}
	// Token's contract is strconv.QuoteToASCII for anything outside its own rune
	// set — a JSON-style escape, so the newline is a two-character sequence in the
	// output rather than a line break. Asserting on the escape rather than merely
	// on the absence of a forged line is what pins the RENDERER: a raw %s would
	// also fail to forge that particular line.
	if !strings.Contains(out, `\n`) {
		t.Errorf("the id was not escaped, so its newline reached the output raw:\n%s", out)
	}
	if !strings.Contains(out, `"A\"B`) {
		t.Errorf("the id was not rendered through the token contract:\n%s", out)
	}
}

// TestReportSourceStringsAreOneLineEach: the report is read by a person and
// pasted into an issue, so a source label that breaks the line structure would
// break both. Source names come from the assembler's vocabulary, but they are
// stored in a text column and a hand-written row can hold anything.
func TestReportSourceStringsAreOneLineEach(t *testing.T) {
	store, projectID, _ := reportStore(t)
	_ = recordCall(t, store, projectID, "search", "USEDID")
	s := newTestSignals(t)
	s.AddProse("the opencode plugin materializes its transcript under mkdtemp")
	judge(t, store, projectID, s)

	rep, err := BuildReport(context.Background(), store, ReportOptions{ProjectID: projectID})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	rep.Sources[0].Source = "search\n- forged: 100% used"

	out := rep.String()
	if strings.Contains(out, "\n- forged: 100%") {
		t.Errorf("a source label forged a line of the report:\n%s", out)
	}
}

// TestBuildStoreReportPoolsProjectsAndNeverSources: the store-wide reader pools
// PROJECTS within a source and never sources with each other, and the direction it
// combines in is the whole of what this package is allowed to combine.
//
// The figures are chosen so summing and averaging cannot be confused: p1 ran three used
// verdicts and p2 ran one ignored one. Summed, the store's search precision is 3 of 4 —
// 75%. Averaged per project it is (100% + 0%) / 2, also a number, and a number about no
// verdict at all: a project with one verdict counts as much as one with three hundred. So
// the counts AND the percentage are both asserted, and an implementation that averaged
// fails both.
//
// The structural properties are asserted alongside, because they are the ones that matter
// on a store with no figures at all: one entry per source, the known sources still named
// in order when empty, no pooled figure reachable, and no project id to print it as.
func TestBuildStoreReportPoolsProjectsAndNeverSources(t *testing.T) {
	store, p1, _ := reportStore(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "p2", "/tmp/audit-report-p2", "p2"); err != nil {
		t.Fatalf("EnsureProject p2: %v", err)
	}
	// Three memories p1's one call kept, all of them restated by the agent's prose
	// — so p1 is 100% used over three verdicts, not over one.
	for _, id := range []string{"A1", "A2", "A3"} {
		seedMemory(t, store, p1, id, memContent)
	}
	// And one only p2's call kept, whose wording the prose never mentions.
	seedMemory(t, store, "p2", "B1", "The ledger reindexes itself after a snapshot restore")

	_ = recordCall(t, store, p1, "search", "A1", "A2", "A3")
	_ = recordCall(t, store, p1, "search")
	_ = recordCall(t, store, "p2", "search", "B1")

	s := newTestSignals(t)
	s.AddProse("the opencode plugin materializes its transcript under mkdtemp")
	judge(t, store, p1, s)
	judge(t, store, "p2", s)

	rep, err := BuildStoreReport(ctx, store, ReportOptions{})
	if err != nil {
		t.Fatalf("BuildStoreReport: %v", err)
	}

	// One entry per source, and only the sources: a reader that appended a total
	// row would answer the question this package exists to refuse.
	seen := map[string]int{}
	for _, src := range rep.Sources {
		seen[src.Source]++
	}
	for name, n := range seen {
		if n != 1 {
			t.Errorf("the store-wide report holds %d entries for %q, want 1", n, name)
		}
	}
	if rep.Pooled() != nil {
		t.Error("the store-wide report can be asked for a pooled figure; the whole rule is that it cannot")
	}
	// The known sources are still all named, in their documented order, even the
	// ones no project has rows for: a store-wide view that dropped them would read
	// as "one source, healthy" rather than "two sources, never measured".
	var order []string
	for _, src := range rep.Sources {
		order = append(order, src.Source)
	}
	if strings.Join(order, ",") != strings.Join(KnownSources, ",") {
		t.Errorf("store-wide sources = %v, want the known sources in order %v", order, KnownSources)
	}

	search := rep.Source("search")
	if search == nil {
		t.Fatal("the store-wide report has no search figures")
	}
	if search.Calls != 3 {
		t.Errorf("store-wide search calls = %d, want 3 (two in p1, one in p2)", search.Calls)
	}
	if search.Kept != 4 || search.Scored != 4 || search.Used != 3 || search.Ignored != 1 {
		t.Errorf("store-wide search kept %d, scored %d, used %d, ignored %d; want 4/4/3/1 — the counts summed, not averaged",
			search.Kept, search.Scored, search.Used, search.Ignored)
	}
	if got, want := search.PrecisionPercent(), 75; got != want {
		t.Errorf("store-wide search precision = %d%%, want %d%%", got, want)
	}
	if search.KeptNothing != 1 {
		t.Errorf("store-wide search kept nothing %d times, want 1 (p1's empty call)", search.KeptNothing)
	}
	// The store-wide report is about no project, so it must not be printable as one.
	if rep.ProjectID != "" {
		t.Errorf("the store-wide report claims project %q; it is a store-wide view", rep.ProjectID)
	}
}

// TestBuildStoreReportDoesNotAliasWhatItRenders: the store-wide report hands its
// renderers slices, and a renderer that sorted or appended one in place would reach the
// next source's line. The reader builds every value fresh per source rather than
// accumulating into a shared slice, and this asserts it on the only two lists a renderer
// can mutate: the contradicted ids and the degradation reasons.
func TestBuildStoreReportDoesNotAliasWhatItRenders(t *testing.T) {
	store, p1, _ := reportStore(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "p2", "/tmp/audit-report-p2", "p2"); err != nil {
		t.Fatalf("EnsureProject p2: %v", err)
	}
	// Distinct ids per project: memory ids are globally unique, so the same id cannot be
	// seeded into two projects — and a fixture that reused one would fail on the UNIQUE
	// constraint rather than on anything this test is about.
	seedMemory(t, store, p1, "ALIAS1", "The v20 migration runs before the pre-migration backup")
	seedMemory(t, store, "p2", "ALIAS2", "The ledger reindexes itself after a snapshot restore")

	_ = recordCall(t, store, p1, "search", "ALIAS1")
	_ = recordCall(t, store, "p2", "search", "ALIAS2")

	s := newTestSignals(t)
	s.AddProse("that is wrong: the v20 migration runs before the pre-migration backup")
	judge(t, store, p1, s)
	judge(t, store, "p2", newTestSignals(t))

	rep, err := BuildStoreReport(ctx, store, ReportOptions{})
	if err != nil {
		t.Fatalf("BuildStoreReport: %v", err)
	}
	// The list is named, and it is this report's own: a second read cannot have grown
	// or reordered it.
	search := rep.Source("search")
	if search == nil {
		t.Fatal("the store-wide report has no search figures")
	}
	first := append([]string(nil), search.ContradictedIDs...)
	firstReasons := append([]string(nil), search.DegradedReasons...)
	// Mutating the rendered list, as a careless renderer would, must not change what the
	// next read produces — which is the whole point of the value being copied per source.
	if len(search.ContradictedIDs) > 0 {
		search.ContradictedIDs[0] = "MUTATED"
	}
	if len(search.DegradedReasons) > 0 {
		search.DegradedReasons[0] = "MUTATED"
	}
	again, err := BuildStoreReport(ctx, store, ReportOptions{})
	if err != nil {
		t.Fatalf("BuildStoreReport (second read): %v", err)
	}
	if got := again.Source("search").ContradictedIDs; !reflect.DeepEqual(got, first) {
		t.Errorf("the contradicted id list changed between reads: %v then %v", first, got)
	}
	if got := again.Source("search").DegradedReasons; !reflect.DeepEqual(got, firstReasons) {
		t.Errorf("the degradation reason list changed between reads: %v then %v", firstReasons, got)
	}
}

// TestReportStringNamesNoScope: the report's own rendering carries no project id,
// because the SCOPE is the caller's to state. `ghost context --audit` states it;
// a merged store-wide view has none to state, and a renderer that printed the field
// unconditionally would label that one "retrieval audit for " — which reads as a
// report whose subject failed to load rather than as a report over the store.
func TestReportStringNamesNoScope(t *testing.T) {
	store, projectID, _ := reportStore(t)
	_ = recordCall(t, store, projectID, "search", "USEDID")
	judge(t, store, projectID, newTestSignals(t))

	rep, err := BuildReport(context.Background(), store, ReportOptions{ProjectID: projectID})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if strings.Contains(rep.String(), projectID) {
		t.Errorf("the report named its own scope:\n%s", rep.String())
	}

	whole, err := BuildStoreReport(context.Background(), store, ReportOptions{})
	if err != nil {
		t.Fatalf("BuildStoreReport: %v", err)
	}
	if whole.ProjectID != "" {
		t.Fatalf("the store-wide report claims project %q; it is about none", whole.ProjectID)
	}
	if strings.Contains(whole.String(), projectID) {
		t.Errorf("the store-wide report printed a project id:\n%s", whole.String())
	}
	// And the window is still on it, because that IS a property of the report.
	if !strings.Contains(whole.String(), "window") {
		t.Errorf("the report dropped its window:\n%s", whole.String())
	}
}

// TestSummaryIsOneLinePerSource: the compact shape, for the block in ghost_health
// where every source is one line. The report's own line() wraps two figures onto a
// second line, which is right there and wrong here — a block that claims one line
// per source and then wraps is a block nobody can scan column-wise.
func TestSummaryIsOneLinePerSource(t *testing.T) {
	cases := []struct {
		name string
		src  SourceReport
		want string
	}{
		{
			name: "no rows",
			src:  SourceReport{Source: "search"},
			want: "search: no rows — this source has recorded no calls",
		},
		{
			name: "calls but no verdicts",
			src:  SourceReport{Source: "search", Calls: 2, Kept: 3},
			want: "search: 2 call(s), 3 kept, no verdict recorded yet for the 3 kept, 0 ignored, 0 superseded in session, 0 contradicted, 0 kept nothing",
		},
		{
			name: "figures",
			src:  SourceReport{Source: "search", Calls: 2, Kept: 4, Scored: 3, Used: 1, Ignored: 2, Superseded: 1, Contradicted: 1, KeptNothing: 1},
			want: "search: 2 call(s), 4 kept, 33% used (1 of 3 scored), 2 ignored, 1 superseded in session, 1 contradicted, 1 kept nothing",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.src.Summary()
			if got != tc.want {
				t.Errorf("Summary() =\n%q\nwant\n%q", got, tc.want)
			}
			if strings.Contains(got, "\n") {
				t.Errorf("Summary() broke the line: %q", got)
			}
		})
	}
}

// TestSummaryQuotesItsOwnSourceLabel: a source name is text in a column, and the
// health block's contract is one line per source. A label with a newline would
// forge a figure, exactly as it would in the report's own line().
func TestSummaryQuotesItsOwnSourceLabel(t *testing.T) {
	src := SourceReport{Source: "search\n- forged: 100% used", Calls: 1, Kept: 1, Scored: 1, Used: 1}
	got := src.Summary()
	if strings.Contains(got, "\n- forged: 100%") {
		t.Errorf("a source label forged a line of the summary: %q", got)
	}
	if strings.Contains(got, "\n") {
		t.Errorf("Summary() broke the line: %q", got)
	}
}

// backdateOnly moves ONE table's recorded_at for a project into the past.
//
// The pair of stamps is the whole problem this file's window has to survive:
// retrieval_record is stamped when the CALL happened and retrieval_audit when the
// detached run JUDGED it, and those are different instants that can be hours
// apart. A window that filters each table by its own column is therefore not one
// window over one population, and backdatingRetrievalRows (both tables at once)
// cannot express the case that matters.
func backdateOnly(t *testing.T, dbPath, table, projectID string, ago time.Duration) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open %s to backdate %s: %v", dbPath, table, err)
	}
	defer db.Close() //nolint:errcheck
	stamp := time.Now().Add(-ago).UTC().Format(memory.StoredStampLayout)
	if _, err := db.Exec(`UPDATE `+table+` SET recorded_at = ? WHERE project_id = ?`, stamp, projectID); err != nil {
		t.Fatalf("backdate %s: %v", table, err)
	}
}

// TestReportCountsAVerdictOnlyWhenItsCallIsCounted: the two tables are stamped at
// two different instants, so one window filter applied to both columns is not one
// population — it is two, and a verdict stamped after its call falls into a window
// its call is not in.
//
// The failure is a figure nobody can read: `search: 0 call(s), 0 kept, 100% used
// (1 of 1 scored)`, i.e. more verdicts than the window admits any memory for, with
// nothing on the report to explain it. So the verdict half is intersected with the
// calls this report actually counted, and the ones left out are counted and named
// rather than dropped in silence.
func TestReportCountsAVerdictOnlyWhenItsCallIsCounted(t *testing.T) {
	store, projectID, dbPath := reportStore(t)
	_ = recordCall(t, store, projectID, "search", "USEDID")
	s := newTestSignals(t)
	s.AddProse("the opencode plugin materializes its transcript under mkdtemp")
	judge(t, store, projectID, s)

	// The call is 72 hours old. The verdict is five minutes old, because that is
	// when the detached run judged it — which is the ordinary case, not an edge.
	backdateOnly(t, dbPath, "retrieval_record", projectID, 72*time.Hour)

	rep, err := BuildReport(context.Background(), store, ReportOptions{ProjectID: projectID, Since: time.Hour})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	search := rep.Source("search")
	if search == nil {
		t.Fatal("the report has no search figures")
	}
	if search.Scored != 0 || search.Used != 0 || search.Ignored != 0 {
		t.Errorf("a verdict whose call fell out of the window is still counted: %+v", *search)
	}
	if search.Kept != 0 || search.Calls != 0 {
		t.Errorf("the call is outside the window and is counted anyway: %+v", *search)
	}
	if search.Detached != 1 {
		t.Errorf("Detached = %d, want 1 — the dropped verdict is counted and named, not lost in silence", search.Detached)
	}
	out := rep.String()
	if !strings.Contains(out, "1 verdict(s) were not counted") {
		t.Errorf("the report does not say a verdict was left out:\n%s", out)
	}
	// A source whose only rows are outside the window is a no-rows source, and the
	// detached verdict must not turn it into a source with figures.
	if !search.NoRows() {
		t.Errorf("the source claims rows it does not have: %+v", *search)
	}
}

// TestReportCountsAVerdictWhoseCallTheStoreNoLongerHolds: the same intersection
// with no window in force. The verdict cap (50000 rows) outlives the call cap (5000
// rows), and #857 exists because a history purge deletes a call and leaves its
// verdicts — so on a busy store this is the NORMAL state, not a window's edge, and
// a report that counted those verdicts would divide a numerator and a denominator
// from two different populations.
func TestReportCountsAVerdictWhoseCallTheStoreNoLongerHolds(t *testing.T) {
	store, projectID, dbPath := reportStore(t)
	_ = recordCall(t, store, projectID, "search", "USEDID")
	s := newTestSignals(t)
	s.AddProse("the opencode plugin materializes its transcript under mkdtemp")
	judge(t, store, projectID, s)

	// The call goes away, exactly as a history purge removes it, and its verdict
	// stays — which is #852's orphan.
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open %s: %v", dbPath, err)
	}
	defer db.Close() //nolint:errcheck
	if _, err := db.Exec(`DELETE FROM retrieval_record WHERE project_id = ?`, projectID); err != nil {
		t.Fatalf("delete the recorded call: %v", err)
	}

	rep, err := BuildReport(context.Background(), store, ReportOptions{ProjectID: projectID})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	search := rep.Source("search")
	if search.Scored != 0 || search.Used != 0 {
		t.Errorf("an orphaned verdict is counted as a use: %+v", *search)
	}
	if search.Detached != 1 {
		t.Errorf("Detached = %d, want 1", search.Detached)
	}
}

// TestReportCountsAVerdictThatNamesNoCallAndSaysSo: the other side of the
// intersection. record_rowid = 0 is a value the write ACCEPTS, for a verdict about
// a session rather than about one call, so it has no call to be in or out of a
// window with. Counting it is right (it is a real verdict about real agent text)
// and hiding it is not, so it is counted in the figures AND named, because it is the
// only way Scored can exceed Kept and a reader who sees that needs to know why.
func TestReportCountsAVerdictThatNamesNoCallAndSaysSo(t *testing.T) {
	store, projectID, _ := reportStore(t)
	_ = recordCall(t, store, projectID, "search", "USEDID")
	if _, err := store.RecordRetrievalAudits(context.Background(), []memory.RetrievalAuditRow{{
		ProjectID: projectID, SessionID: "s1", Source: "search", MemoryID: "IGNID",
		Outcome: string(OutcomeIgnored), RecordRowID: 0,
	}}); err != nil {
		t.Fatalf("RecordRetrievalAudits: %v", err)
	}

	rep, err := BuildReport(context.Background(), store, ReportOptions{ProjectID: projectID})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	search := rep.Source("search")
	// COUNTED and NAMED, but in no figure. The other half of the property is the
	// test's own point: an unattributed verdict must not reach Scored, because
	// precision is a ratio over (call, memory) pairs and this row has no call to be
	// one of — a numerator and a denominator from two populations.
	if search.Unattributed != 1 {
		t.Errorf("Unattributed = %d, want 1 — a verdict naming no call must still be counted", search.Unattributed)
	}
	if search.Scored != 0 || search.Used != 0 || search.Ignored != 0 {
		t.Errorf("a verdict naming no call reached a figure: %+v", *search)
	}
	if search.Detached != 0 {
		t.Errorf("Detached = %d, want 0 — a verdict naming no call is unattributed, not detached", search.Detached)
	}
	if percent, ok := search.Precision(); ok {
		t.Errorf("Precision() reported %d%% over a source whose only verdict is unattributed, so there is nothing in the ratio", percent)
	}
	out := rep.String()
	if !strings.Contains(out, "no call") {
		t.Errorf("the report does not name the unattributed verdict:\n%s", out)
	}
	// And the note says WHY it is out of the figures, so a reader does not read the
	// omission as a lost row.
	if !strings.Contains(out, "no call to be one of") {
		t.Errorf("the note does not say the unattributed verdict is out of the figures rather than lost:\n%s", out)
	}
}

// TestTheDegradedNoteCountsVerdictsNotMemories: the note says how much of the
// denominator rests on a partly-read transcript, so its denominator has to be the
// verdicts. Kept is admitted memories, which is a larger number whenever an audit is
// merely incomplete — and the health block already divides by Scored, so a report
// dividing by Kept makes the two surfaces of one figure disagree.
func TestTheDegradedNoteCountsVerdictsNotMemories(t *testing.T) {
	store, projectID, _ := reportStore(t)
	_ = recordCall(t, store, projectID, "search", "USEDID", "IGNID", "CONID")

	recs, err := store.RetrievalRecordsForProject(context.Background(), projectID, 0)
	if err != nil || len(recs) != 1 {
		t.Fatalf("RetrievalRecordsForProject: %v (%d records)", err, len(recs))
	}
	if _, err := store.RecordRetrievalAudits(context.Background(), []memory.RetrievalAuditRow{{
		ProjectID: projectID, SessionID: "s1", Source: "search", MemoryID: "USEDID",
		Outcome: string(OutcomeUsed), Signal: "identifier", Degraded: "transcript truncated",
		RecordRowID: recs[0].RowID,
	}}); err != nil {
		t.Fatalf("RecordRetrievalAudits: %v", err)
	}

	rep, err := BuildReport(context.Background(), store, ReportOptions{ProjectID: projectID})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	search := rep.Source("search")
	if search.Kept != 3 || search.Scored != 1 || search.DegradedVerdicts != 1 {
		t.Fatalf("fixture: %+v", *search)
	}
	out := rep.String()
	if !strings.Contains(out, "1 of its 1 scored verdict(s)") {
		t.Errorf("the degraded note divides by kept memories instead of verdicts:\n%s", out)
	}
	if strings.Contains(out, "1 of its 3 verdict(s)") {
		t.Errorf("the degraded note reports a verdict count the store does not hold:\n%s", out)
	}
}

// TestTheDegradedReasonIsRenderedAsALabel: the degraded reason is a stored TEXT
// column, and the report already labels the source label on the very same line -- so a
// reason carrying a newline forges a line of the report, and a forged line here is a
// FIGURE, not a message: the next line of a report is another source's numbers.
//
// The report is the surface where a mistake is hardest to catch, which is the whole
// reason it labels what it stores. This pins the other half of that rule, on both
// places the reason is printed.
func TestTheDegradedReasonIsRenderedAsALabel(t *testing.T) {
	store, projectID, dbPath := reportStore(t)
	_ = recordCall(t, store, projectID, "search", "USEDID", "IGNID")

	recs, err := store.RetrievalRecordsForProject(context.Background(), projectID, 0)
	if err != nil || len(recs) != 1 {
		t.Fatalf("RetrievalRecordsForProject: %v (%d records)", err, len(recs))
	}
	if _, err := store.RecordRetrievalAudits(context.Background(), []memory.RetrievalAuditRow{{
		ProjectID: projectID, SessionID: "s1", Source: "search", MemoryID: "USEDID",
		Outcome: string(OutcomeUsed), Signal: "identifier",
		RecordRowID: recs[0].RowID,
	}}); err != nil {
		t.Fatalf("RecordRetrievalAudits: %v", err)
	}

	// The reason is written the way a hand-edited or newer row would carry it: a
	// newline, then a line that reads exactly like the report's own figures. It is
	// planted through the column rather than through MarkDegraded because that is
	// the point -- nothing in this package constrains what the column holds.
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open %s: %v", dbPath, err)
	}
	defer db.Close() //nolint:errcheck
	hostile := "scan transcript: truncated\n  project_context: 99 call(s), 100% used (99 of 99 scored)"
	if _, err := db.Exec(`UPDATE retrieval_audit SET degraded = ? WHERE project_id = ?`, hostile, projectID); err != nil {
		t.Fatalf("plant the hostile reason: %v", err)
	}

	rep, err := BuildReport(context.Background(), store, ReportOptions{ProjectID: projectID})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}

	// Both surfaces: the per-source line() and the report-level String(). A fix to
	// one of them is the bug this test is about.
	for name, out := range map[string]string{"String()": rep.String(), "SourceReport.line()": rep.Source("search").line()} {
		// The fix ensures the newline is escaped (as \n) so the output remains
		// a single logical line per source -- no new line is forged. We verify
		// that the hostile line does not appear as a SEPARATE line in the output.
		lines := strings.Split(out, "\n")
		for _, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "project_context: 99 call(s)") {
				t.Errorf("%s: the degraded reason forged a new line: %q", name, line)
			}
		}
		// Also verify the store doesn't hold this figure at all -- it should only
		// appear escaped on the degraded line, not as a standalone line.
		for _, line := range lines {
			if strings.Contains(line, "99 of 99 scored") && !strings.Contains(line, "partly-read") && !strings.Contains(line, "degraded") {
				t.Errorf("%s printed a standalone figure the store does not hold: %q", name, line)
			}
		}
	}
}

// TestBuildStoreReportIsTheSumOfThePerProjectReports: the whole-store reader and the
// per-project reader must be ONE arithmetic, not two that agree today.
//
// They are two implementations — one counts in Go over whole tables, the other is a
// GROUP BY per table — and a second implementation of one figure is how this package
// already produced one bug: the degraded note divided by Kept while the health block
// divided by Scored, and the two surfaces of the same number disagreed for a release.
// So the property is asserted as EQUALITY against the per-project reports rather than
// against numbers picked by hand, over a store whose two halves are shaped differently
// on purpose: p1 has a session_start injection, p2 does not, p1 has a degraded verdict,
// p2 has one naming no call.
//
// A store-wide report that quietly disagreed here would be invisible in review, because
// both numbers would look reasonable.
func TestBuildStoreReportIsTheSumOfThePerProjectReports(t *testing.T) {
	store, p1, dbPath := reportStore(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "p2", "/tmp/audit-report-p2", "p2"); err != nil {
		t.Fatalf("EnsureProject p2: %v", err)
	}
	for _, id := range []string{"A1", "A2", "A3"} {
		seedMemory(t, store, p1, id, memContent)
	}
	// A4 gets its OWN wording, and that is the reason it is not simply a fourth copy of
	// memContent. The superseded_in_session bucket is a save restating a memory's
	// distinctive words, so a save written for one of four identical memories would
	// supersede all four — and a fixture that cannot supersede exactly one memory cannot
	// tell an aggregate that maps the bucket from one that maps it for everything. This
	// fixture filed no superseded verdict at all before that, and that is how the store-wide
	// reader came to match a bucket named "superseded" against the stored
	// "superseded_in_session" for a release.
	seedMemory(t, store, p1, "A4", "The relay listens on port 2222 in production")
	seedMemory(t, store, "p2", "B1", "The ledger reindexes itself after a snapshot restore")
	seedMemory(t, store, "p2", "B2", "Bench seeds restore content through the shared clamp helper")

	// p1: a search keeping four, an injection keeping one, and a search keeping none.
	_ = recordCall(t, store, p1, "search", "A1", "A2", "A3", "A4")
	_ = recordCall(t, store, p1, "session_start", "A1")
	_ = recordCall(t, store, p1, "search")
	// p2: a search keeping two and an injection keeping nothing.
	_ = recordCall(t, store, "p2", "search", "B1", "B2")
	_ = recordCall(t, store, "p2", "session_start")

	// p1 judged cleanly, p2 judged under a partial transcript read — so the degraded
	// reason is named on one source and not the other. p1's signals carry one SAVE, which
	// restates A4's wording and supersedes exactly that memory.
	p1Signals := newTestSignals(t)
	p1Signals.AddSaveArgs("the relay listens on port 2222 in production")
	judge(t, store, p1, p1Signals)
	degraded := newTestSignals(t)
	degraded.MarkDegraded("scan transcript: truncated")
	judge(t, store, "p2", degraded)

	// A verdict naming no call — in NO figure, and the shape a store-wide reader can
	// plausibly get wrong, because it lands in a bucket without touching a call.
	// A verdict naming no call at all, which is the shape the write accepts for a
	// verdict about a session rather than about one call.
	fileVerdict(t, store, memory.RetrievalAuditRow{
		ProjectID: "p2", SessionID: "s9", Source: "search", MemoryID: "B2",
		Outcome: string(OutcomeIgnored), RecordRowID: 0,
	})
	// The unattributed one is written; the DETACHED one is planted by SQL, because
	// #857's writer refuses a verdict naming a call that did not keep that memory — so
	// this build's writer cannot produce the shape at all, and the only ways to reach
	// it are the call cap evicting a call under the verdict cap's pressure (5000 rows
	// against 50000) and a store written before #857.
	plantRawVerdict(t, dbPath,
		`INSERT INTO retrieval_audit (project_id, record_rowid, session_id, source, memory_id, outcome, signal, degraded)
		 VALUES ('p2', 999999, 's9', 'search', 'B1', 'contradicted', '', '')`)

	whole, err := BuildStoreReport(ctx, store, ReportOptions{})
	if err != nil {
		t.Fatalf("BuildStoreReport: %v", err)
	}

	perProject := make([]Report, 0, 2)
	for _, p := range []string{p1, "p2"} {
		rep, err := BuildReport(ctx, store, ReportOptions{ProjectID: p})
		if err != nil {
			t.Fatalf("BuildReport %s: %v", p, err)
		}
		perProject = append(perProject, rep)
	}
	// The reference side is spelled out HERE rather than shipped as a combining function
	// over []Report: a combiner with no production caller is how the next reader
	// concludes it is the sanctioned way to pool, which is the one thing this package
	// refuses to be able to do.
	summed := sumPerProject(perProject)

	if whole.Pooled() != nil || summed.Pooled() != nil {
		t.Error("a store-wide report can be asked for a pooled figure; the whole rule is that it cannot")
	}
	// The same sources, in the same order, so a difference below is arithmetic and not
	// a source the whole-store path found or lost.
	var wholeOrder, sumOrder []string
	for _, src := range whole.Sources {
		wholeOrder = append(wholeOrder, src.Source)
	}
	for _, src := range summed.Sources {
		sumOrder = append(sumOrder, src.Source)
	}
	if strings.Join(wholeOrder, ",") != strings.Join(sumOrder, ",") {
		t.Fatalf("the store-wide report holds %v and the summed one %v; the comparison below would compare different sources", wholeOrder, sumOrder)
	}

	for _, name := range wholeOrder {
		w, s := whole.Source(name), summed.Source(name)
		if w == nil || s == nil {
			t.Fatalf("source %q is missing from one of the two reports", name)
		}
		if diff := diffSourceReports(*w, *s); diff != "" {
			t.Errorf("source %q: the store-wide reader and the per-project reports disagree — this is the bug the two implementations share or do not share:\n%s", name, diff)
		}
	}
}

// diffSourceReports renders the fields of two figures that disagree, or "" when none do.
// Every count is compared, and both id lists, because a renderer that prints one of them
// must not be able to show a different value from the one the count says.
func diffSourceReports(want, got SourceReport) string {
	var b []string
	note := func(field string, w, g any) {
		if fmt.Sprint(w) != fmt.Sprint(g) {
			b = append(b, fmt.Sprintf("%s: whole-store %v, per-project sum %v", field, w, g))
		}
	}
	note("Calls", want.Calls, got.Calls)
	note("Kept", want.Kept, got.Kept)
	note("KeptNothing", want.KeptNothing, got.KeptNothing)
	note("Scored", want.Scored, got.Scored)
	note("Used", want.Used, got.Used)
	note("Ignored", want.Ignored, got.Ignored)
	note("Superseded", want.Superseded, got.Superseded)
	note("Contradicted", want.Contradicted, got.Contradicted)
	note("DegradedVerdicts", want.DegradedVerdicts, got.DegradedVerdicts)
	note("Unattributed", want.Unattributed, got.Unattributed)
	note("Detached", want.Detached, got.Detached)
	note("DegradedReasons", want.DegradedReasons, got.DegradedReasons)
	note("ContradictedIDs", want.ContradictedIDs, got.ContradictedIDs)
	// And the rendered line, because two figures can agree field-by-field and still
	// render differently — which is the shape the degraded-denominator bug had.
	if wl, gl := want.Summary(), got.Summary(); wl != gl {
		b = append(b, fmt.Sprintf("Summary(): whole-store %q, per-project sum %q", wl, gl))
	}
	out := ""
	for _, s := range b {
		out += "\n  " + s
	}
	return out
}

// sumPerProject is the reference side of the equality test: per-project reports added up
// WITHIN each source, by SUM of counts and never by averaging percentages.
//
// It is a test helper rather than an exported function on purpose. A shipped combiner over
// []Report with no production caller is how the next reader concludes it is the sanctioned
// way to pool — which is the one thing this package refuses to be able to do — so the sum
// lives where its only caller is.
func sumPerProject(reports []Report) Report {
	bySource := map[string]*SourceReport{}
	for _, rep := range reports {
		for _, src := range rep.Sources {
			cur := bySource[src.Source]
			if cur == nil {
				entry := src
				// Copied, not aliased, so summing cannot reach back into a caller's
				// report through a shared slice.
				entry.ContradictedIDs = append([]string(nil), src.ContradictedIDs...)
				entry.DegradedReasons = append([]string(nil), src.DegradedReasons...)
				bySource[src.Source] = &entry
				continue
			}
			cur.Calls += src.Calls
			cur.Kept += src.Kept
			cur.KeptNothing += src.KeptNothing
			cur.Scored += src.Scored
			cur.Used += src.Used
			cur.Ignored += src.Ignored
			cur.Superseded += src.Superseded
			cur.Contradicted += src.Contradicted
			cur.DegradedVerdicts += src.DegradedVerdicts
			cur.Unattributed += src.Unattributed
			cur.Detached += src.Detached
			cur.ContradictedIDs = mergeSortedNames(cur.ContradictedIDs, src.ContradictedIDs)
			cur.DegradedReasons = mergeSortedNames(cur.DegradedReasons, src.DegradedReasons)
		}
	}
	summed := Report{}
	for _, name := range sortedKeys(bySource) {
		summed.Sources = append(summed.Sources, *bySource[name])
	}
	return Report{Sources: orderSources(summed.Sources)}
}

// mergeSortedNames is the union of two already-sorted lists.
func mergeSortedNames(a, b []string) []string {
	if len(a) == 0 {
		return append([]string(nil), b...)
	}
	if len(b) == 0 {
		return a
	}
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, list := range [][]string{a, b} {
		for _, s := range list {
			if seen[s] {
				continue
			}
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// plantRawVerdict writes a verdict row directly, for the shapes the WRITER refuses.
//
// #857 made RecordRetrievalAudits refuse a verdict naming a call that did not keep that
// memory, which is right and also means a store written by this build alone cannot hold a
// detached verdict. It can still hold one — the call cap evicts at 5000 rows while the
// verdict cap holds 50000 — so a test about that state has to plant it, and doing it
// through the writer would be testing that the writer refuses it, which is a different
// test that internal/memory already has.
func plantRawVerdict(t *testing.T, dbPath, stmt string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open %s: %v", dbPath, err)
	}
	defer db.Close() //nolint:errcheck
	if _, err := db.Exec(stmt, args...); err != nil {
		t.Fatalf("plant the raw verdict: %v", err)
	}
}
