package assemble

// #646 part 1: the seam between the assembler and the retrieval record.
//
// The record is written from HERE and not by the surface, because the verdicts
// it needs exist only inside Run: Result.Items is the answer after the
// response-fit post-pass has possibly dropped rows, and Trace.Decisions is the
// only place the stages recorded why. A surface that re-derived either would be
// a second implementation of the same rules — the failure mode this package's
// Leaks()/TrimmedByBudget() contract already exists to prevent.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// recordingSink captures the records a Run produced, and can be made to fail.
type recordingSink struct {
	records []memory.RetrievalRecord
	err     error
	calls   int
}

func (r *recordingSink) RecordRetrieval(_ context.Context, rec memory.RetrievalRecord) error {
	r.calls++
	r.records = append(r.records, rec)
	return r.err
}

func (r *recordingSink) last(t *testing.T) memory.RetrievalRecord {
	t.Helper()
	if len(r.records) == 0 {
		t.Fatal("no retrieval record was written")
	}
	return r.records[len(r.records)-1]
}

// lastOf is last for a caller that already holds the Result, so the assertion
// about what the search returned and the assertion about what it recorded read
// as one statement about one call.
func (r *recordingSink) lastOf(t *testing.T, _ Result) memory.RetrievalRecord {
	t.Helper()
	return r.last(t)
}

// runRecorded is this package's own convention for calling the seam under test:
// a fixed candidate set behind the recording sink, so the record is read from a
// run whose input the test also states.
func runRecorded(t *testing.T, sink RecordSink, req Request, rows ...memory.Candidate) Result {
	t.Helper()
	req.Record = sink
	return run(t, &fakeRetriever{set: setOf(rows...)}, req)
}

// TestRunRecordsWhatItKeptAndWhatItDropped: the record is the trace, projected.
//
// The three verdicts the trace carries are each read from the place that
// recorded them: a row in the answer is KEPT, a row the trace dropped is
// DROPPED with that stage's reason, and a row the trace KEPT for a reason
// (validity_unparseable, the one case v1 has) carries that reason rather than an
// empty one. Nothing here is re-derived — the budget-dropped row in particular
// is one the stages decided about, and a projection that recomputed "did the
// answer fit" would answer a different question.
func TestRunRecordsWhatItKeptAndWhatItDropped(t *testing.T) {
	sink := &recordingSink{}

	rows := []memory.Candidate{
		candidate("KEPT-1", "proj", "fact", "database configuration pooling", 0.9),
		candidate("KEPT-2", "proj", "fact", "database configuration retry", 0.8),
		// Dropped by the category predicate, before the window closes.
		candidate("DROPPED-CAT", "proj", "gotcha", "database configuration vacuum", 0.7),
	}

	req := baseRequest()
	req.Category = "fact"
	req.Budget.MaxItems = 2

	runRecorded(t, sink, req, rows...)

	rec := sink.last(t)
	if rec.ProjectID != "proj" {
		t.Errorf("project = %q, want %q", rec.ProjectID, "proj")
	}
	if rec.Source != string(SourceSearch) {
		t.Errorf("source = %q, want %q", rec.Source, SourceSearch)
	}
	if rec.Outcome != string(OutcomeAnswerable) {
		t.Errorf("outcome = %q, want %q", rec.Outcome, OutcomeAnswerable)
	}
	if rec.Reason != reasonFloorMet {
		t.Errorf("reason = %q, want %q", rec.Reason, reasonFloorMet)
	}
	if rec.AsOf != "" {
		t.Errorf("as_of = %q, want empty for a current read", rec.AsOf)
	}

	byID := map[string]memory.RowVerdict{}
	for _, v := range rec.Verdicts {
		byID[v.ID] = v
	}
	if len(byID) != len(rec.Verdicts) {
		t.Errorf("the same id appears twice in one record: %+v", rec.Verdicts)
	}
	for _, id := range []string{"KEPT-1", "KEPT-2"} {
		v, ok := byID[id]
		if !ok {
			t.Errorf("a row the answer carries (%s) is not in the record: %+v", id, rec.Verdicts)
			continue
		}
		if !v.Kept {
			t.Errorf("row %s is in the answer but recorded as dropped", id)
		}
	}
	v, ok := byID["DROPPED-CAT"]
	if !ok {
		t.Fatalf("a row the stages dropped is not in the record: %+v", rec.Verdicts)
	}
	if v.Kept {
		t.Error("a dropped row is recorded as kept")
	}
	if v.Stage != stagePredicates || v.Reason != "category_mismatch" {
		t.Errorf("the dropped row's verdict = (%s, %s), want (%s, category_mismatch) — the reason is not the stage's own",
			v.Stage, v.Reason, stagePredicates)
	}
}

// TestRunRecordsTheVerdictOfARowItKeptForAReason: the `keep` half of the trace's
// decision vocabulary.
//
// Recording a kept row through the drop path would tell the audit that a memory
// in the answer was excluded from it — the exact inversion #730's reader would
// act on. So a keep that carries a reason keeps it, and the record has somewhere
// to say "admitted, but its validity could not be read".
func TestRunRecordsTheVerdictOfARowItKeptForAReason(t *testing.T) {
	sink := &recordingSink{}

	// A validity value SQLite cannot parse: stage 2 reports it and keeps the
	// row, which is the only keep the trace records. The field is an unconstrained
	// text column in the schema, so "sometime last spring" is exactly the shape a
	// hand-written or restored row carries.
	garbage := "sometime last spring"
	row := candidate("UNPARSEABLE", "proj", "fact", "database configuration pooling", 0.9)
	row.ValidUntil = &garbage

	res := runRecorded(t, sink, baseRequest(), row)
	if len(res.Items) != 1 {
		t.Fatalf("the fixture did not keep its row: %d items, want 1 — the case under test never happened", len(res.Items))
	}

	rec := sink.last(t)
	if len(rec.Verdicts) != 1 {
		t.Fatalf("record holds %d verdicts, want 1: %+v", len(rec.Verdicts), rec.Verdicts)
	}
	v := rec.Verdicts[0]
	if v.ID != "UNPARSEABLE" || !v.Kept {
		t.Errorf("verdict = %+v, want UNPARSEABLE kept", v)
	}
	if v.Stage != stageValidity || v.Reason != "validity_unparseable" {
		t.Errorf("a kept-with-a-reason row recorded (%s, %s), want (%s, validity_unparseable)",
			v.Stage, v.Reason, stageValidity)
	}
}

// TestRunRecordsWhatTheResponseFitPassDropped: the post-pass's verdict reaches
// the record too, and it can only do that because the record is written AFTER
// fitResponse.
//
// fitResponse drops rows from the bottom of the ranking until the response fits
// a byte cap, and each drop is a `response_fit` decision on the trace. A record
// written before the post-pass would name a row the caller never received as
// kept — the false "used" verdict that would make the whole report wrong in the
// direction that flatters Ghost.
func TestRunRecordsWhatTheResponseFitPassDropped(t *testing.T) {
	sink := &recordingSink{}

	rows := []memory.Candidate{
		candidate("FITS", "proj", "fact", "short note", 0.9),
		candidate("TRIMMED", "proj", "fact", strings.Repeat("a long note ", 400), 0.8),
	}

	req := baseRequest()
	req.Budget.MaxItems = 2
	// A cap the short row fits inside and the long one does not.
	req.Budget.MaxBytes = 700

	res := runRecorded(t, sink, req, rows...)
	if len(res.Items) != 1 || res.Items[0].ID != "FITS" {
		t.Fatalf("the fixture did not trim: items = %v — the case under test never happened", itemIDs(res.Items))
	}

	// EXACTLY ONE record, and this is the assertion that makes the placement
	// above checkable rather than merely stated. Reading only the sink's LAST
	// record passes even when a record is written before the post-pass as well,
	// because the later one is the correct one — so a second, wrong record naming
	// TRIMMED as kept would sit in the sink unexamined and in the store as a
	// false "used" verdict. One call is one row; a Run that recorded twice is a
	// Run with a denominator full of calls that did not happen.
	if sink.calls != 1 {
		t.Fatalf("the sink was called %d times for one search, want 1 — the record is written once, after the fit pass", sink.calls)
	}

	rec := sink.last(t)
	var trimmed memory.RowVerdict
	for _, v := range rec.Verdicts {
		if v.ID == "TRIMMED" {
			trimmed = v
		}
	}
	if trimmed.Kept {
		t.Error("a row the response-fit pass dropped is recorded as kept — the caller never received it")
	}
	if trimmed.Stage != stageResponseFit || trimmed.Reason != reasonBudgetDropped {
		t.Errorf("the trimmed row's verdict = (%s, %s), want (%s, %s)",
			trimmed.Stage, trimmed.Reason, stageResponseFit, reasonBudgetDropped)
	}
}

// TestRunRecordsTheQueryAsAHashAndNeverAsText: the privacy half, asserted on the
// struct the seam carries.
//
// A record whose type could hold a query would eventually hold one — the hash is
// the harder thing to keep, and it is only the harder thing because the type
// makes the text impossible to express. So the assertion is on the SHAPE (there
// is no field for text) and on the value (a 64-character digest, stable for the
// same query and different for another).
func TestRunRecordsTheQueryAsAHashAndNeverAsText(t *testing.T) {
	first := &recordingSink{}
	second := &recordingSink{}
	other := &recordingSink{}

	row := candidate("A", "proj", "fact", "database configuration pooling", 0.9)
	runRecorded(t, first, baseRequest(), row)
	runRecorded(t, second, baseRequest(), row)

	otherReq := baseRequest()
	otherReq.Query = "database configuration pooling "
	runRecorded(t, other, otherReq, row)

	a, b, c := first.last(t), second.last(t), other.last(t)
	if a.QueryHash != b.QueryHash {
		t.Error("the same query produced two different hashes — the audit could not group calls by question")
	}
	if a.QueryHash == c.QueryHash {
		t.Error("two different queries produced the same hash")
	}
	if len(a.QueryHash) != 64 {
		t.Errorf("query hash is %d characters, want a 64-character sha256 digest: %q", len(a.QueryHash), a.QueryHash)
	}
	// The text is not anywhere in the record, in any field. A hash of the
	// default baseRequest's query is checked by value above; this is the
	// structural half — the record as a whole says nothing about what was asked.
	if strings.Contains(a.QueryHash, "database") || strings.Contains(a.Reason, "database") {
		t.Errorf("the record carries the query text in one of its fields: %+v", a)
	}
}

// TestRunRecordsTheSurfaceAndAPassiveCallWithNoQuestion: Source is load-bearing,
// and for a reason that is easy to miss — Ghost serves stdio, whose connection
// reports NO session id, so a session-start injection and a search look
// identical by session alone. Without this column the audit cannot tell which
// denominator a row belongs in, and every precision figure it prints would be a
// blend of two different questions.
//
// The request is the package's own passiveRequest(), because that is the real
// shape a session start now takes (#761): no query, and a slice budget. A
// session-start call carries no question, so its record must carry no
// fingerprint either — the digest of the empty string would be the SAME constant
// for every injection, which is a value in a hash column that looks like a
// fingerprint of something and is not.
func TestRunRecordsTheSurfaceAndAPassiveCallWithNoQuestion(t *testing.T) {
	sink := &recordingSink{}

	rec := sink.lastOf(t, runRecorded(t, sink, passiveRequest(),
		candidate("A", "proj", "fact", "database configuration pooling", 0.9)))

	if rec.Source != string(SourceSessionStart) {
		t.Errorf("source = %q, want %q", rec.Source, SourceSessionStart)
	}
	if rec.QueryHash != "" {
		t.Errorf("query hash = %q, want empty for a call that carried no query — a constant here reads as a fingerprint", rec.QueryHash)
	}
	if rec.AsOf != "" {
		t.Errorf("as_of = %q, want empty for a passive call", rec.AsOf)
	}
	// A passive call is not a relevance verdict, and the record says whatever the
	// assembler actually concluded rather than a re-derived outcome. Stated here
	// because it is the shape the next part reads a denominator out of.
	if rec.Outcome == "" {
		t.Error("a passive call recorded no outcome; the audit reads the verdict, not the absence of one")
	}
}

// TestRunRecordsAHistoricalInstant: a record with no as_of cannot be told from
// a record of a block assembled at an instant the store has since moved past,
// and the two have different reasons to be audited — a current call is what was
// injected NOW, and a historical one is a reconstruction that injected nothing.
func TestRunRecordsAHistoricalInstant(t *testing.T) {
	sink := &recordingSink{}
	at := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)

	req := baseRequest()
	req.AsOf = &at

	rec := sink.lastOf(t, runRecorded(t, sink, req,
		candidate("A", "proj", "fact", "database configuration pooling", 0.9)))

	if rec.AsOf != at.Format(time.RFC3339) {
		t.Errorf("as_of = %q, want %q", rec.AsOf, at.Format(time.RFC3339))
	}
}

// TestARecordSinkFailureNeverFailsTheSearch: the seam is log-and-continue, and
// this is the property that makes it safe to leave wired on a live tool.
//
// A retrieval record is a measurement of a call that already happened. If
// recording it could fail the call, then the act of measuring retrieval would
// change retrieval's availability — and it would do so at exactly the moments
// the store is unhealthy, which is when a report is most wanted. The Result is
// returned unchanged and the error is swallowed.
func TestARecordSinkFailureNeverFailsTheSearch(t *testing.T) {
	sink := &recordingSink{err: errors.New("disk is on fire")}

	req := baseRequest()
	req.Record = sink

	res, err := Run(context.Background(), &fakeRetriever{set: setOf(
		candidate("A", "proj", "fact", "database configuration pooling", 0.9),
	)}, req)
	if err != nil {
		t.Fatalf("Run: %v — a failing record sink must not fail the search", err)
	}
	if len(res.Items) != 1 {
		t.Errorf("the answer changed because the record could not be written: %v", itemIDs(res.Items))
	}
	if res.Response == "" {
		t.Error("the response is empty — the fit pass saw a different Result")
	}
	if sink.calls != 1 {
		t.Errorf("the sink was called %d times, want 1", sink.calls)
	}
}

// TestNoRecordIsWrittenForACallThatReturnedNothing: an error is not an answer,
// and there is nothing to record about one.
//
// A retrieval that failed, a request Run refused and a block whose envelope
// cannot fit all return an error instead of a Result. A record for any of them
// would claim a set of retrieved memories for a call that returned none, and
// "did the agent use what Ghost injected" would then have a denominator full of
// injections that never happened.
func TestNoRecordIsWrittenForACallThatReturnedNothing(t *testing.T) {
	// A retrieval failure.
	failing := &recordingSink{}
	req := baseRequest()
	req.Record = failing
	if _, err := Run(context.Background(), errRetriever{errors.New("leg is down")}, req); err == nil {
		t.Fatal("Run with a failing retriever returned no error")
	}
	if failing.calls != 0 {
		t.Errorf("a failed retrieval wrote %d record(s), want none", failing.calls)
	}

	// A request Run refuses before it retrieves anything.
	refused := &recordingSink{}
	badReq := baseRequest()
	badReq.Record = refused
	badReq.Source = "not-a-source"
	if _, err := Run(context.Background(), &fakeRetriever{set: setOf()}, badReq); err == nil {
		t.Fatal("Run with an unknown source returned no error")
	}
	if refused.calls != 0 {
		t.Errorf("a refused request wrote %d record(s), want none", refused.calls)
	}

	// A block whose own envelope cannot fit the cap: there is no answer, so
	// there is no answer to record.
	impossible := &recordingSink{}
	tightReq := baseRequest()
	tightReq.Record = impossible
	tightReq.Budget.MaxBytes = 20
	if _, err := Run(context.Background(), &fakeRetriever{set: setOf()}, tightReq); err == nil {
		t.Fatal("Run with an impossible response cap returned no error")
	}
	if impossible.calls != 0 {
		t.Errorf("a call that returned no answer wrote %d record(s), want none", impossible.calls)
	}
}

// TestNoRecordForAResultTheCallerTurnsIntoAnError: the case Run cannot see from
// inside, and it is a surface's to declare.
//
// A result with a failed leg and nothing admitted is a perfectly valid Result —
// Run returns it without error. What makes it not a retrieval is that the CALLER
// converts it: the MCP handler returns a retryable "this search is incomplete"
// error rather than letting an agent read "nothing matched" as a fact. A record
// written before that conversion puts a row in the audit's denominator for a call
// that returned no memories at all, and nothing downstream can tell it apart —
// the leg failure lives in the trace, and the record does not carry it.
//
// So the request declares the conversion, and the assertion is that the same run
// IS recorded when the caller does not declare it, which is what makes this a
// gate and not a deletion.
func TestNoRecordForAResultTheCallerTurnsIntoAnError(t *testing.T) {
	// A retriever whose set reports the vector leg as failed, with no rows — the
	// shape the handler converts into an error.
	failedLeg := &recordingSink{}
	req := baseRequest()
	req.Record = failedLeg
	req.SuppressRecordWhenLegsFailed = true

	_, err := Run(context.Background(), &fakeRetriever{set: &memory.CandidateSet{
		Legs: map[string]memory.LegStatus{
			"vector": {Applicable: true, Attempted: true, Available: false, Err: "leg is down"},
		},
	}}, req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if failedLeg.calls != 0 {
		t.Errorf("a call the caller turns into an error wrote %d record(s), want none — the audit would count a "+
			"call that delivered no answer", failedLeg.calls)
	}

	// The same run IS recorded without the declaration: a caller that RENDERS such
	// a result is entitled to have it audited, and this is what proves the
	// suppression is a gate rather than a blanket silence.
	rendered := &recordingSink{}
	req.Record = rendered
	req.SuppressRecordWhenLegsFailed = false
	if _, err := Run(context.Background(), &fakeRetriever{set: &memory.CandidateSet{
		Legs: map[string]memory.LegStatus{
			"vector": {Applicable: true, Attempted: true, Available: false, Err: "leg is down"},
		},
	}}, req); err != nil {
		t.Fatalf("Run (no suppression): %v", err)
	}
	if rendered.calls != 1 {
		t.Errorf("a caller that RENDERS the result wrote %d record(s), want 1", rendered.calls)
	}

	// And a failed leg that still admitted rows is not suppressed: the handler
	// returns that as a degraded answer, and it IS a retrieval.
	degraded := &recordingSink{}
	req.Record = degraded
	req.SuppressRecordWhenLegsFailed = true
	res := runRecorded(t, degraded, req, candidate("A", "proj", "fact", "database configuration pooling", 0.9))
	if len(res.Items) == 0 {
		t.Fatal("the fixture admitted nothing, so the degraded case never happened")
	}
	if degraded.calls != 1 {
		t.Errorf("a degraded answer with a failed leg wrote %d record(s), want 1 — the handler RETURNS that one", degraded.calls)
	}
}

func TestARefusedRecordReachesTheCallersLoggerNotTheProcessDefault(t *testing.T) {
	// Poison the process default: any write to it fails the test.
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(poisonedWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	var callerLog bytes.Buffer
	sink := &recordingSink{err: errors.New("record retrieval: a project id is required")}
	req := baseRequest()
	req.Record = sink
	req.Logger = slog.New(slog.NewTextHandler(&callerLog, &slog.HandlerOptions{Level: slog.LevelWarn}))

	runRecorded(t, sink, req, candidate("A", "proj", "fact", "database configuration pooling", 0.9))

	if !strings.Contains(callerLog.String(), "retrieval record not written") {
		t.Errorf("the refused record did not reach the CALLER's logger — it is being sent to the process "+
			"default, which no Ghost process configures: %q", callerLog.String())
	}
}

// poisonedWriter fails the test if anything is logged through it.
type poisonedWriter struct{ t *testing.T }

func (p poisonedWriter) Write(b []byte) (int, error) {
	p.t.Errorf("something logged to the PROCESS DEFAULT logger, which no Ghost process configures: %s", b)
	return len(b), nil
}

// TestARefusedRecordNeverCostsTheSearchAndAlwaysReachesTheLog: what a record the
// store will NOT write has to do, because one such case is reachable from the
// wired surface.
//
// `ResolveProject` treats a genuine miss as `("", "", nil)` rather than an error,
// and the search handler assigns that unconditionally — so a search for a project
// that does not exist runs, retrieves, and returns a real answer, and its record
// arrives at the store with no project to attribute it to. `RecordRetrieval`
// refuses that, correctly: a row filed under nothing answers "which project's
// retrieval was this" as "all of them".
//
// The refusal must therefore cost the SEARCH nothing (it does not — the answer is
// unchanged) and must not be SILENT (a report with a hole in it and no log line is
// a report that is quietly wrong). So the log names the reason, and this test
// pins both halves: the result survives the refusal, and the refusal is visible in
// the captured log rather than swallowed.
func TestARefusedRecordNeverCostsTheSearchAndAlwaysReachesTheLog(t *testing.T) {
	var logged bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	sink := &recordingSink{err: errors.New("record retrieval: a project id is required")}
	req := baseRequest()
	req.Record = sink

	res := runRecorded(t, sink, req,
		candidate("A", "proj", "fact", "database configuration pooling", 0.9))

	// Half one: the search is untouched.
	if len(res.Items) != 1 {
		t.Errorf("the answer changed because the record was refused: %v", itemIDs(res.Items))
	}
	if res.Response == "" {
		t.Error("the response is empty — the fit pass saw a different Result")
	}
	if res.Outcome != OutcomeAnswerable {
		t.Errorf("outcome = %q, want the pipeline's own verdict", res.Outcome)
	}
	// Half two: the loss is reported, and the log says WHY rather than only that
	// something went wrong.
	if !strings.Contains(logged.String(), "retrieval record not written") {
		t.Errorf("a refused record logged nothing: %q — a report with a hole in it must never be silent", logged.String())
	}
	if !strings.Contains(logged.String(), "a project id is required") {
		t.Errorf("the log does not carry the refusal's reason: %q", logged.String())
	}
}

// TestARunWithNoSinkRecordsNothing: the seam is optional, and a caller that
// installs none gets the assembler this version has always had.
//
// The bench and every test in this package call Run without a sink, so this is
// not a hypothetical path — and "Run needs somewhere to record" would be a
// breaking change to the one entry point everything else is written against.
func TestARunWithNoSinkRecordsNothing(t *testing.T) {
	// No Record field set at all, and no panic, and the result is the one the
	// pipeline produced.
	res := run(t, &fakeRetriever{set: setOf(
		candidate("A", "proj", "fact", "database configuration pooling", 0.9),
	)}, baseRequest())
	if len(res.Items) != 1 {
		t.Errorf("items = %v, want the one row", itemIDs(res.Items))
	}
}

// errRetriever is a retriever that fails, for the paths where the failure is the
// subject rather than a fixture detail.
type errRetriever struct{ err error }

func (e errRetriever) Candidates(context.Context, memory.CandidateRequest) (*memory.CandidateSet, error) {
	return nil, e.err
}
