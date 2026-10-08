package audit

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// auditStore opens a real store: the runner's contract is with SQLite, not with
// an interface, so a fake here would prove nothing about the write seam, the
// newer-store refusal or the replace-in-place semantics.
func auditStore(t *testing.T) (*memory.Store, string) {
	t.Helper()
	ctx := context.Background()
	db, err := memory.OpenDB(filepath.Join(t.TempDir(), "ghost.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := memory.NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := store.EnsureProject(ctx, "p1", "/tmp/audit-run-p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	return store, "p1"
}

// seedMemory stores content under a chosen id, because the verdicts name ids and
// a random one would make the test's expectations unreadable.
//
// The category and source are set because the store's CHECKs require them and a
// fixture that failed to seed would fail every assertion below for a reason that
// has nothing to do with the audit. Neither is read by the comparison.
func seedMemory(t *testing.T, store *memory.Store, projectID, id, content string) {
	t.Helper()
	if _, err := store.CreateWithID(context.Background(), projectID, id, memory.Memory{
		Content:  content,
		Category: "fact",
		Source:   "manual",
	}); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

// recordCall writes one retrieval record whose kept rows are the given ids, and returns
// ITS OWN ROWID.
//
// The rowid comes back from the write rather than from a later read because #857's write
// guard files a verdict only against a call that kept THAT memory, and it REFUSES the
// rest by returning them rather than by erroring. So a fixture that reached for "the
// newest row" instead of "the row this call is" would have its verdict silently dropped
// and would then assert on zeroes that agree with each other for the wrong reason.
func recordCall(t *testing.T, store *memory.Store, projectID, source string, kept ...string) int64 {
	t.Helper()
	verdicts := make([]memory.RowVerdict, 0, len(kept))
	for _, id := range kept {
		verdicts = append(verdicts, memory.RowVerdict{ID: id, Kept: true, Stage: "fit", Reason: "fit_response"})
	}
	if err := store.RecordRetrieval(context.Background(), memory.RetrievalRecord{
		ProjectID: projectID,
		SessionID: testSession,
		Source:    source,
		Outcome:   "answerable",
		Verdicts:  verdicts,
	}); err != nil {
		t.Fatalf("RecordRetrieval: %v", err)
	}
	return newestCallRowID(t, store, projectID)
}

// newestCallRowID is the rowid of the call just written.
func newestCallRowID(t *testing.T, store *memory.Store, projectID string) int64 {
	t.Helper()
	recs, err := store.RetrievalRecordsForProject(context.Background(), projectID, 0)
	if err != nil || len(recs) == 0 {
		t.Fatalf("RetrievalRecordsForProject(%s): %v (%d records)", projectID, err, len(recs))
	}
	return recs[0].RowID
}

// fileVerdict files verdicts and FAILS if the store refused any.
//
// A refusal is not an error — the rows simply are not stored — so only the fixture can
// see it, and a fixture that ignored it would assert on a store missing the very rows
// the test is about. The two return values are the whole reason: a report that already
// counted these rows into its figures has to be able to take a refusal back out.
func fileVerdict(t *testing.T, store *memory.Store, rows ...memory.RetrievalAuditRow) {
	t.Helper()
	refused, err := store.RecordRetrievalAudits(context.Background(), rows)
	if err != nil {
		t.Fatalf("RecordRetrievalAudits: %v", err)
	}
	if len(refused) > 0 {
		t.Fatalf("the fixture's verdicts were REFUSED and never stored: %+v", refused)
	}
}

// TestRunJudgesEveryVerdict is the end-to-end property the issue asks for: what
// a retrieval kept, against what the agent did with it.
func TestRunJudgesEveryVerdict(t *testing.T) {
	store, projectID := auditStore(t)
	seedMemory(t, store, projectID, "USEDID", memContent)
	seedMemory(t, store, projectID, "SUPID", "Pinned versions come from the lockfile, never a floating tag")
	seedMemory(t, store, projectID, "CONID", "The v20 migration runs before the pre-migration backup")
	seedMemory(t, store, projectID, "IGNID", "Bench seeds restore content through the shared clamp helper")
	_ = recordCall(t, store, projectID, "search", "USEDID", "SUPID", "CONID", "IGNID")

	s := newTestSignals(t)
	s.AddProse("as I read it, the opencode plugin materializes its transcript under mkdtemp")
	s.AddSaveArgs("pinned versions come from the lockfile and never a floating tag")
	s.AddProse("that is wrong: the v20 migration runs after the pre-migration backup")

	res, err := Run(context.Background(), store, projectID, s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := res.byMemory()
	want := map[string]Outcome{
		"USEDID": OutcomeUsed,
		"SUPID":  OutcomeSuperseded,
		"CONID":  OutcomeContradicted,
		"IGNID":  OutcomeIgnored,
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s = %q, want %q", id, got[id], w)
		}
	}
	if res.Verdicts != 4 {
		t.Errorf("Verdicts = %d, want 4 — one per kept memory", res.Verdicts)
	}
}

// TestRunStampsTheHashOfTheContentItJudged: the writer's half of #879 (schema
// v22). The reader withdraws a verdict whose stamp is not the hash of what is
// stored now, so the stamp has to be the hash of the TEXT this run compared —
// taken from the content map the comparison itself read, not re-read from the
// store afterwards. A stamp of anything else withdraws every verdict on every
// later pass, silently, and that is the failure this test cannot see from the
// report's side: the run above still prints four figures while the rows it
// wrote are already unreadable to the reader.
func TestRunStampsTheHashOfTheContentItJudged(t *testing.T) {
	store, projectID := auditStore(t)
	const content = "Pinned versions come from the lockfile, never a floating tag"
	seedMemory(t, store, projectID, "SUPID", content)
	_ = recordCall(t, store, projectID, "search", "SUPID")

	s := newTestSignals(t)
	s.AddSaveArgs("pinned versions come from the lockfile and never a floating tag")
	res, err := Run(context.Background(), store, projectID, s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Verdicts != 1 {
		t.Fatalf("Verdicts = %d, want 1 — the fixture judged nothing, so no row carries a stamp", res.Verdicts)
	}
	rows, err := store.RetrievalAudits(context.Background(), projectID, "")
	if err != nil {
		t.Fatalf("RetrievalAudits: %v", err)
	}
	if len(rows) != 1 || rows[0].MemoryID != "SUPID" {
		t.Fatalf("stored %+v, want one verdict naming SUPID", rows)
	}
	if rows[0].Outcome != string(OutcomeSuperseded) {
		t.Errorf("outcome = %q, want %q — the fixture's verdict moved under the stamp it is asserting",
			rows[0].Outcome, OutcomeSuperseded)
	}
	if want := memory.ContentHash(content); rows[0].ContentHash != want {
		t.Errorf("content_hash = %q, want the hash of the text the comparison read (%q): every verdict this "+
			"run wrote would be withdrawn by the reader on the next pass", rows[0].ContentHash, want)
	}
}

// TestRunSkipsMemoriesItCannotRead: a kept memory deleted after the call cannot
// be judged, and a verdict about a row that no longer exists is a claim about
// nothing. It is counted as unreadable rather than silently dropped, so the
// summary can say the run was partial.
func TestRunSkipsMemoriesItCannotRead(t *testing.T) {
	store, projectID := auditStore(t)
	seedMemory(t, store, projectID, "HERE", memContent)
	_ = recordCall(t, store, projectID, "search", "HERE", "GONE")

	s := newTestSignals(t)
	s.AddProse("the opencode plugin materializes its transcript under mkdtemp")

	res, err := Run(context.Background(), store, projectID, s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Unreadable != 1 {
		t.Errorf("Unreadable = %d, want 1", res.Unreadable)
	}
	if res.Verdicts != 1 {
		t.Errorf("Verdicts = %d, want 1 — the missing memory gets no verdict", res.Verdicts)
	}
}

// TestRunDoesNotJudgeDroppedRows: only a KEPT memory reached the agent, so a
// dropped one can have been neither used nor ignored. Judging it would report a
// retrieval failure that never happened to anybody.
func TestRunDoesNotJudgeDroppedRows(t *testing.T) {
	store, projectID := auditStore(t)
	seedMemory(t, store, projectID, "KEPT", memContent)
	seedMemory(t, store, projectID, "DROPPED", memContent)
	if err := store.RecordRetrieval(context.Background(), memory.RetrievalRecord{
		ProjectID: projectID,
		SessionID: testSession,
		Source:    "search",
		Outcome:   "answerable",
		Verdicts: []memory.RowVerdict{
			{ID: "KEPT", Kept: true, Stage: "fit", Reason: "fit_response"},
			{ID: "DROPPED", Kept: false, Stage: "validity", Reason: "expired"},
		},
	}); err != nil {
		t.Fatalf("RecordRetrieval: %v", err)
	}

	s := newTestSignals(t)
	s.AddProse("the opencode plugin materializes its transcript under mkdtemp")
	res, err := Run(context.Background(), store, projectID, s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Verdicts != 1 {
		t.Errorf("Verdicts = %d, want 1: a dropped row was judged", res.Verdicts)
	}
	if res.byMemory()["DROPPED"] != "" {
		t.Error("a dropped row received a verdict")
	}
}

// TestRunFilesOneVerdictPerCallAndMemory is the table's GRAIN, asserted where it
// is easiest to get wrong. One memory kept by two calls is two verdicts against
// two different calls — not one, and not one per memory — because the row's
// record_rowid is what makes the replace idempotent and what lets a report say
// which surface the agent ignored the memory on.
//
// A run-wide de-duplication by memory id would satisfy every other test in this
// file and be wrong here: it would collapse the two rows into one, and the
// session-start's figure would be filed against the search's rowid.
func TestRunFilesOneVerdictPerCallAndMemory(t *testing.T) {
	store, projectID := auditStore(t)
	seedMemory(t, store, projectID, "M1", memContent)
	_ = recordCall(t, store, projectID, "search", "M1")
	_ = recordCall(t, store, projectID, "session_start", "M1")

	s := newTestSignals(t)
	s.AddProse("the opencode plugin materializes its transcript under mkdtemp")
	if _, err := Run(context.Background(), store, projectID, s); err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows, err := store.RetrievalAudits(context.Background(), projectID, "")
	if err != nil {
		t.Fatalf("RetrievalAudits: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d verdict(s), want one per (call, memory) pair", len(rows))
	}
	bySource := map[string]int64{}
	for _, r := range rows {
		bySource[r.Source] = r.RecordRowID
	}
	if len(bySource) != 2 {
		t.Fatalf("verdicts filed under %v, want one row per source", bySource)
	}
	if bySource["search"] == bySource["session_start"] {
		t.Error("both verdicts name the same call; each must be filed against the call that kept the memory")
	}
	if bySource["search"] == 0 || bySource["session_start"] == 0 {
		t.Errorf("a verdict is unattributable: %v", bySource)
	}
}

// TestRunIsIdempotentOverTheSameCall is what makes it safe to run from the stop
// hook, which fires after every turn: the second run REPLACES the first's
// verdicts for the same call instead of adding to them.
func TestRunIsIdempotentOverTheSameCall(t *testing.T) {
	store, projectID := auditStore(t)
	seedMemory(t, store, projectID, "M1", memContent)
	_ = recordCall(t, store, projectID, "search", "M1")

	silent := newTestSignals(t)
	silent.AddProse("worked on something unrelated entirely")
	if _, err := Run(context.Background(), store, projectID, silent); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if got := mustRead(t, store, projectID); got["M1"] != OutcomeIgnored {
		t.Fatalf("first run = %q, want %q", got["M1"], OutcomeIgnored)
	}

	talkative := newTestSignals(t)
	talkative.AddProse("the opencode plugin materializes its transcript under mkdtemp, so the sidecar is synchronous")
	if _, err := Run(context.Background(), store, projectID, talkative); err != nil {
		t.Fatalf("second run: %v", err)
	}
	got := mustRead(t, store, projectID)
	if got["M1"] != OutcomeUsed {
		t.Errorf("second run = %q, want %q", got["M1"], OutcomeUsed)
	}
	if len(got) != 1 {
		t.Errorf("two runs left %d verdict(s), want 1: the second replaced the first", len(got))
	}
}

// TestRunReplacesOnlyTheCallsItJudged: two calls, and the second run's window
// may cover only the newer one. The older call's verdicts must survive — they
// were judged against a different transcript, and nothing about them is stale.
func TestRunReplacesOnlyTheCallsItJudged(t *testing.T) {
	store, projectID := auditStore(t)
	seedMemory(t, store, projectID, "M1", memContent)
	seedMemory(t, store, projectID, "M2", memContent)
	_ = recordCall(t, store, projectID, "search", "M1")
	_ = recordCall(t, store, projectID, "search", "M2")

	s := newTestSignals(t)
	s.AddProse("the opencode plugin materializes its transcript under mkdtemp")
	if _, err := Run(context.Background(), store, projectID, s); err != nil {
		t.Fatalf("run both: %v", err)
	}

	// A later run whose window covers only the newest call.
	if err := store.RecordRetrieval(context.Background(), memory.RetrievalRecord{
		ProjectID: projectID, SessionID: testSession, Source: "search", Outcome: "answerable",
		Verdicts: []memory.RowVerdict{{ID: "M1", Kept: true, Stage: "fit", Reason: "fit_response"}},
	}); err != nil {
		t.Fatalf("RecordRetrieval: %v", err)
	}
	restore := CallWindow
	CallWindow = 1
	t.Cleanup(func() { CallWindow = restore })
	if _, err := Run(context.Background(), store, projectID, newTestSignals(t)); err != nil {
		t.Fatalf("narrow run: %v", err)
	}
	got := mustRead(t, store, projectID)
	if len(got) == 0 {
		t.Fatal("the narrow run deleted verdicts for a call it never judged")
	}
}

// TestRunReportsSearchesAndSessionStartsSeparately is the denominator rule: the
// two surfaces answer different questions, so their figures are never pooled.
func TestRunReportsSearchesAndSessionStartsSeparately(t *testing.T) {
	store, projectID := auditStore(t)
	seedMemory(t, store, projectID, "M1", memContent)
	_ = recordCall(t, store, projectID, "search", "M1")
	_ = recordCall(t, store, projectID, "session_start", "M1")

	s := newTestSignals(t)
	s.AddProse("the opencode plugin materializes its transcript under mkdtemp")
	res, err := Run(context.Background(), store, projectID, s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Sources) != 2 {
		t.Fatalf("got %d source figure(s), want one per source", len(res.Sources))
	}
	for _, src := range res.Sources {
		if src.Calls != 1 || src.Used != 1 {
			t.Errorf("source %q = %+v, want 1 call and 1 use", src.Source, src)
		}
	}
}

// TestRunCountsSearchesThatKeptNothing is the DETECTABLE half of "missed": a
// lookup the agent made that admitted no memory at all. The other half the issue
// names — a fact the agent re-derived in-session that was never injected — is
// not derivable from a transcript by any heuristic, so it is not counted here and
// the summary says so rather than guessing.
func TestRunCountsSearchesThatKeptNothing(t *testing.T) {
	store, projectID := auditStore(t)
	_ = recordCall(t, store, projectID, "search")
	_ = recordCall(t, store, projectID, "search", "M1")
	seedMemory(t, store, projectID, "M1", memContent)

	res, err := Run(context.Background(), store, projectID, newTestSignals(t))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Verdicts != 1 {
		t.Errorf("Verdicts = %d, want 1", res.Verdicts)
	}
	var src *SourceSummary
	for i := range res.Sources {
		if res.Sources[i].Source == "search" {
			src = &res.Sources[i]
		}
	}
	if src == nil {
		t.Fatal("no search figures")
	}
	if src.KeptNothing != 1 {
		t.Errorf("KeptNothing = %d, want 1", src.KeptNothing)
	}
	// The other half of "missed" — a fact the agent re-derived in-session that
	// was never injected — has no field, because no heuristic can find it: a
	// transcript cannot distinguish a fact it was never told from one it worked
	// out. It is a printed LIMIT on the summary, so a reader of the figure knows
	// what the number does not cover.
	if printed := res.String(); !strings.Contains(printed, "re-derived") {
		t.Errorf("the summary does not state the undetectable half of \"missed\":\n%s", printed)
	}
}

// TestRunRefusesAnEmptyProject: a run against nothing would persist nothing and
// say nothing, which reads downstream as "the audit found no problems".
func TestRunRefusesAnEmptyProject(t *testing.T) {
	store, _ := auditStore(t)
	if _, err := Run(context.Background(), store, "", newTestSignals(t)); err == nil {
		t.Error("Run accepted an empty project id")
	}
}

// TestRunStandsOnWhatItReads: the runner's own output must not carry the
// transcript, the query or a memory's content — the whole feature's constraint,
// asserted on the summary a caller would print.
func TestRunStandsOnWhatItReads(t *testing.T) {
	store, projectID := auditStore(t)
	const secret = "the vacuum schedule runs at four in the morning"
	seedMemory(t, store, projectID, "M1", secret)
	_ = recordCall(t, store, projectID, "search", "M1")

	s := newTestSignals(t)
	s.AddProse("unrelated")
	res, err := Run(context.Background(), store, projectID, s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	printed := res.String()
	for _, word := range []string{"vacuum", "schedule", "morning", secret} {
		if strings.Contains(printed, word) {
			t.Errorf("the summary carries %q: the report is ids and counts", word)
		}
	}
}

// TestRunStoresTheTranscriptDegradation: a partially read transcript produces
// "ignored" verdicts that are claims about text it never saw. The degradation
// rides with each verdict so a reader can discount them.
func TestRunStoresTheTranscriptDegradation(t *testing.T) {
	store, projectID := auditStore(t)
	seedMemory(t, store, projectID, "M1", memContent)
	_ = recordCall(t, store, projectID, "search", "M1")

	s := newTestSignals(t)
	s.MarkDegraded("scan transcript: read failure mid-transcript")
	res, err := Run(context.Background(), store, projectID, s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Degraded != "scan transcript: read failure mid-transcript" {
		t.Errorf("Degraded = %q, want the scanner's reason", res.Degraded)
	}
	rows, err := store.RetrievalAudits(context.Background(), projectID, "")
	if err != nil {
		t.Fatalf("RetrievalAudits: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d verdict(s), want 1", len(rows))
	}
	if !strings.Contains(rows[0].Degraded, "read failure mid-transcript") {
		t.Errorf("the stored verdict does not carry the degradation: %q", rows[0].Degraded)
	}
}

// mustRead returns every stored verdict for the project, keyed by memory id.
func mustRead(t *testing.T, store *memory.Store, projectID string) map[string]Outcome {
	t.Helper()
	rows, err := store.RetrievalAudits(context.Background(), projectID, "")
	if err != nil {
		t.Fatalf("RetrievalAudits: %v", err)
	}
	out := make(map[string]Outcome, len(rows))
	for _, r := range rows {
		out[r.MemoryID] = Outcome(r.Outcome)
	}
	return out
}

// byMemory indexes a summary's figures by memory id.
func (r Summary) byMemory() map[string]Outcome {
	out := map[string]Outcome{}
	for _, v := range r.VerdictList {
		out[v.MemoryID] = v.Outcome
	}
	return out
}

// TestTheRunSummaryNamesTheEmptyCallCountAfterItsSource: the lifecycle summary `ghost
// lifecycle` prints to stderr has the same per-source line this package's report has,
// and the same closing sentence — "only searches that kept nothing are reported as
// missed" — so it has the same way of being misread, and it was fixed by the same
// helper.
//
// Asserted on the string the command actually prints rather than on the Summary
// struct's fields: the finding was never that the figure was wrong, it was that every
// source's figure carried a search's NAME.
func TestTheRunSummaryNamesTheEmptyCallCountAfterItsSource(t *testing.T) {
	store, projectID, _ := reportStore(t)
	// One of each, so both figures are in the same summary and each line has to
	// carry its own name.
	_ = recordCall(t, store, projectID, "search")
	_ = recordCall(t, store, projectID, "session_start")

	res, err := Run(context.Background(), store, projectID, newTestSignals(t))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := res.String()

	searchLine := sourceLine(t, out, "search")
	if !strings.Contains(searchLine, "1 kept nothing") {
		t.Errorf("the search line does not say it kept nothing:\n%s", searchLine)
	}
	startLine := sourceLine(t, out, "session_start")
	if !strings.Contains(startLine, "1 admitted nothing") {
		t.Errorf("the injection line does not say it admitted nothing:\n%s", startLine)
	}
	if strings.Contains(startLine, "kept nothing") {
		t.Errorf("the injection line names its figure as a failed lookup:\n%s", startLine)
	}
}

// TestTheRunSummarySplitsUsedByItsSignal: the lifecycle summary says what proved each
// positive verdict, in the same phrasing the report prints, and carries the same caveat.
func TestTheRunSummarySplitsUsedByItsSignal(t *testing.T) {
	store, projectID, _ := reportStore(t)
	_ = recordCall(t, store, projectID, "search", "USEDID", "IGNID", "CONID")

	s := newTestSignals(t)
	s.AddID("USEDID")
	s.AddProse("the v20 migration runs before the pre-migration backup")
	res, err := Run(context.Background(), store, projectID, s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	src := res.Sources[0]
	if src.Used != 2 || src.UsedByID != 1 || src.UsedByWording != 1 {
		t.Fatalf("Used/UsedByID/UsedByWording = %d/%d/%d, want 2/1/1", src.Used, src.UsedByID, src.UsedByWording)
	}
	line := sourceLine(t, res.String(), "search")
	if !strings.Contains(line, "33% cited by id (1 of 3 scored), 1 restated by wording (heuristic), 1 ignored") {
		t.Errorf("the summary line does not split used by its signal:\n%s", line)
	}
	if !strings.Contains(res.String(), LimitsSentence) {
		t.Errorf("the summary does not carry the caveat sentence:\n%s", res.String())
	}
}

// TestTheRunSummaryRendersItsSourceThroughLabel: the lifecycle summary prints one
// line per source, and `src.Source` is the retrieval_record.source column verbatim —
// plain TEXT with no CHECK, validated by nothing, and as ordinary a thing to receive
// as a row from a restored backup.
//
// This is the third renderer of this exact line, and the other two both render the
// column through assemble.Label for the reason report.go says: a raw source is a
// string a stored row chooses, and one holding a newline forges a figure of its own.
// It matters more here than in the report, because `ghost lifecycle` writes this to
// stderr as the phase tail an operator reads in lifecycle.log. Asserted twice, for
// the two ways a renderer can be wrong: the forged line must not appear, and the
// label must be the escaped form (a raw %s would also fail the first assertion for
// this particular payload only by luck).
func TestTheRunSummaryRendersItsSourceThroughLabel(t *testing.T) {
	store, projectID, _ := reportStore(t)
	_ = recordCall(t, store, projectID, "search")

	res, err := Run(context.Background(), store, projectID, newTestSignals(t))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Written onto the figure the Run just produced, rather than through a second
	// Run with a hostile source: this is the RENDERER under test, and a hostile row
	// would also have to survive RecordRetrieval first.
	res.Sources[0].Source = "search\n- forged: 100% used"

	out := res.String()
	if strings.Contains(out, "\n- forged: 100%") {
		t.Errorf("a source label forged a line of the lifecycle summary:\n%s", out)
	}
	if !strings.Contains(out, `\n`) {
		t.Errorf("the source was not escaped, so its newline reached the log raw:\n%s", out)
	}
}

// TestTheRunSummaryRendersItsScopeAndDegradationAsLabels: the summary this package's
// String() returns is printed to stderr by `ghost lifecycle`, as the phase tail an
// operator reads in lifecycle.log. Three values in it are STORED text, and each is a
// string a row or a restored file chose:
//
//   - ProjectID, a projects-row value. A hand-edited or restored database can hold one
//     this build's import checkers would refuse, and a newline forges a line of the
//     summary.
//   - src.Source, the retrieval_record.source column — its own test above.
//   - Degraded, the scan's reason. `MarkDegraded`'s contract says the argument is a
//     reason from the hook's fail-open vocabulary and never a transcript phrase, but the
//     one caller builds it with `%v` of an error, and the sidecar format quotes rather
//     than rejects, so a newline survives a save/load round trip.
//
// All three now go through assemble.Label, which is what the two sibling renderers
// already do for the same two of them: `report.go` labels its source and its degraded
// reasons (labelDegraded), and `printContextAudit` labels the report's project id with
// `TestTheAuditScopeLineRendersTheProjectAsALabel` pinning it. The failure mode is the
// same in each case — a stored string becomes a line of figures — so the hardening has
// to cover the whole function or none of it.
//
// Two payloads, distinct, so each site is pinned separately: a single forged substring
// would satisfy "no forged line" for both if both were left raw and only one escaped.
func TestTheRunSummaryRendersItsScopeAndDegradationAsLabels(t *testing.T) {
	store, projectID, _ := reportStore(t)
	_ = recordCall(t, store, projectID, "search")

	res, err := Run(context.Background(), store, projectID, newTestSignals(t))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Written onto the summary the Run just produced, rather than through a second
	// Run with hostile inputs: this is the RENDERER under test, and a hostile value
	// would also have to survive whatever wrote it.
	res.ProjectID = projectID + "\n- forged-scope: 100% used"
	res.Degraded = "transcript truncated\n- forged-degraded: 100% used"

	out := res.String()
	for _, forged := range []string{"\n- forged-scope: 100%", "\n- forged-degraded: 100%"} {
		if strings.Contains(out, forged) {
			t.Errorf("a stored value forged a line of the lifecycle summary (%q):\n%s", forged, out)
		}
	}
	if !strings.Contains(out, `\n- forged-scope`) || !strings.Contains(out, `\n- forged-degraded`) {
		t.Errorf("neither value was escaped, so its newline reached the log raw:\n%s", out)
	}
}
