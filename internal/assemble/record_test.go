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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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

// DigestQuery stands in for the store's keyed digest. It is computed from the
// query with a FIXED test key and touches no file, which is the point of asking
// the sink for it: nothing in this package can create a per-install key on the
// machine the suite runs on, and a test that could would be a test writing to a
// developer's real data directory.
func (r *recordingSink) DigestQuery(query string) (string, error) { return testDigest(query) }

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

// TestRunRecordsTheVerdictOfARowItKeptForAReason: a kept row that carries a
// reason keeps it, and is not recorded through the drop path (#730).
func TestRunRecordsTheVerdictOfARowItKeptForAReason(t *testing.T) {
	sink := &recordingSink{}

	// A validity value SQLite cannot parse: stage 2 reports it and keeps the row,
	// which is the only keep the trace records.
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

	// EXACTLY ONE record, and this is the assertion that makes the placement above
	// checkable rather than merely stated.
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

// TestRunRecordsTheSurfaceAndAPassiveCallWithNoQuestion: a session-start call
// (#761) records its source and carries no query fingerprint.
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
	// assembler actually concluded rather than a re-derived outcome.
	if rec.Outcome == "" {
		t.Error("a passive call recorded no outcome; the audit reads the verdict, not the absence of one")
	}
}

// TestNoRecordForAResultTheCallerTurnsIntoAnError: the case Run cannot see from
// inside, and it is a surface's to declare.
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

	// The same run IS recorded without the declaration: a caller that RENDERS such a
	// result is entitled to have it audited.
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

// TestAFailedRecordLogWithholdsATokenShapedProjectID is #1464: the drop log line
// must withhold a token-shaped stored project id and still name an ordinary one.
func TestAFailedRecordLogWithholdsATokenShapedProjectID(t *testing.T) {
	// Obviously fake, shaped by the detector's own GitHub rule. The length is
	// the PAT's: "ghp_" + 36.
	const tokenID = "ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	var logged bytes.Buffer
	sink := &recordingSink{err: errors.New("disk full")}

	req := baseRequest()
	req.ProjectID = tokenID
	req.Logger = slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelWarn}))

	runRecorded(t, sink, req, candidate("A", tokenID, "fact", "database configuration pooling", 0.9))

	line := logged.String()
	if !strings.Contains(line, "retrieval record not written") {
		t.Fatalf("the drop was not logged at all: %q", line)
	}
	if strings.Contains(line, tokenID) {
		t.Errorf("the drop log repeats the token-shaped project id: %s", line)
	}
	// The line has to stay actionable: a bare "withheld" that named neither the field
	// nor the format would tell an operator nothing about which project's.
	if !strings.Contains(line, "<project_id withheld: it holds a GitHub personal access token>") {
		t.Errorf("the drop log does not name the withheld project id and its format: %s", line)
	}

	// The ordinary half, from a second run on the same logger: an id with no token in
	// it must still appear so the line can name the project whose record was dropped.
	var ordinary bytes.Buffer
	okSink := &recordingSink{err: errors.New("disk full")}
	okReq := baseRequest()
	okReq.ProjectID = "proj"
	okReq.Logger = slog.New(slog.NewTextHandler(&ordinary, &slog.HandlerOptions{Level: slog.LevelWarn}))
	runRecorded(t, okSink, okReq, candidate("A", "proj", "fact", "database configuration pooling", 0.9))

	if !strings.Contains(ordinary.String(), "proj") {
		t.Errorf("an ordinary project id no longer appears in the drop log: %s", ordinary.String())
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

// testDigest is the fixed-key digest every sink fake in this file returns, so the
// fakes share one implementation and none of them touches the filesystem.
func testDigest(query string) (string, error) {
	if query == "" {
		return "", nil
	}
	mac := hmac.New(sha256.New, []byte("test-key"))
	mac.Write([]byte(query))
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// poisonedWriter fails the test if anything is logged through it.
type poisonedWriter struct{ t *testing.T }

func (p poisonedWriter) Write(b []byte) (int, error) {
	p.t.Errorf("something logged to the PROCESS DEFAULT logger, which no Ghost process configures: %s", b)
	return len(b), nil
}

// blockingSink stands in for a store that will not give up the write lock: it
// blocks until its context is done, which is what a BEGIN IMMEDIATE waiting out
// a busy_timeout looks like from the caller's side.
type blockingSink struct {
	waited  chan struct{}
	dropped bool
}

func (b *blockingSink) DigestQuery(query string) (string, error) { return testDigest(query) }

func (b *blockingSink) RecordRetrieval(ctx context.Context, _ memory.RetrievalRecord) error {
	close(b.waited)
	<-ctx.Done()
	b.dropped = true
	return ctx.Err()
}

// TestARecordWriteCannotDelayTheAnswerPastItsBudget: the write is best-effort,
// and this is the property that makes leaving it on a live tool acceptable.
func TestARecordWriteCannotDelayTheAnswerPastItsBudget(t *testing.T) {
	var logged bytes.Buffer
	sink := &blockingSink{waited: make(chan struct{})}

	req := baseRequest()
	req.Record = sink
	req.Logger = slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelWarn}))

	start := time.Now()
	res := run(t, &fakeRetriever{set: setOf(
		candidate("A", "proj", "fact", "database configuration pooling", 0.9),
	)}, req)
	elapsed := time.Since(start)

	// The answer is intact — that is the half that matters.
	if len(res.Items) != 1 {
		t.Errorf("items = %v, want the one row: a record that could not be written changed the answer",
			itemIDs(res.Items))
	}
	if res.Response == "" {
		t.Error("the response is empty — the fit pass saw a different Result")
	}
	// The sink was reached, and released at the budget rather than left hanging.
	select {
	case <-sink.waited:
	default:
		t.Fatal("the record write was never attempted")
	}
	if !sink.dropped {
		t.Error("the sink was not released by its deadline")
	}
	// And the wait is bounded. The budget is 250ms; allow generous slack for a
	// loaded machine but nothing like the store's 5s x2.
	if elapsed > 5*recordWriteBudget {
		t.Errorf("Run took %v with a blocked record write, want at most ~%v — the record write is "+
			"holding up the answer", elapsed, 5*recordWriteBudget)
	}
	// The loss is reported, not silent.
	if !strings.Contains(logged.String(), "retrieval record not written") {
		t.Errorf("the dropped record logged nothing: %q", logged.String())
	}
}
