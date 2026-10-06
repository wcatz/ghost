package resolve

// #648 slice 1: resolve receives the audit's NEGATIVE evidence as INPUT.
//
// The evidence is a fact about a memory's past, told to the classifier beside the
// note it is about — and nothing else about resolve changes. What may be
// resolved, what is cached, what the veto settles and what the pass writes are
// all as they were; the only difference is one extra line inside the text the
// harness is asked about.
//
// The two properties these tests hold are the ones that can rot silently: a
// `used` verdict must be invisible (it is the #284 popularity loop the moment a
// reader puts it in a prompt), and a memory with no audit rows must produce
// byte-for-byte the input it produced before any of this existed.

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// errEvidenceUnavailable stands in for a store that cannot answer the read. Its
// text is never asserted: what matters is that it is an error the pass survives.
var errEvidenceUnavailable = errors.New("evidence unavailable")

// resolveRealStore is a real store holding three resolve-eligible notes, because
// the property under test is what the pass SENDS, and a fake store's usefulness
// map would only prove the pass renders whatever it is handed.
func resolveRealStore(t *testing.T) (*memory.Store, context.Context, []memory.Memory) {
	t.Helper()
	ctx := context.Background()
	db, err := memory.OpenDB(filepath.Join(t.TempDir(), "resolve.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := memory.NewStore(db, nil)
	const project = "usefulness"
	if err := s.EnsureProject(ctx, project, "/tmp/usefulness", project); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	// Every note passes the keyword prefilter (so all three reach the classifier)
	// and none is settled by the veto (which reads an imperative or an open
	// marker — none of these carry one), because a note the pass never asks
	// about is a note this slice cannot say anything about.
	contents := []string{
		"kill experiment: the graph bonus is gone, PR #210 landed it",
		"postmortem concluded: the deploy failure was a stale hash",
		"cost estimate from May: $148 per month projected",
	}
	var rows []memory.Memory
	for _, content := range contents {
		id, err := s.Create(ctx, project, memory.Memory{Category: "fact", Content: content, Importance: 0.5, Source: "mcp"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		stored, err := s.GetByIDs(ctx, []string{id})
		if err != nil || len(stored) != 1 {
			t.Fatalf("GetByIDs(%s): %v", id, err)
		}
		rows = append(rows, stored[0])
	}
	return s, ctx, rows
}

// TestAMemoryWithNoAuditRowsIsSentByteForByteAsToday: the promise every other
// consumer of this feature rests on. A project that has never been audited must
// see exactly the input it saw before this slice, so the change is inert where
// there is nothing to say and cannot be blamed for a verdict that moved.
func TestAMemoryWithNoAuditRowsIsSentByteForByteAsToday(t *testing.T) {
	s, ctx, rows := resolveRealStore(t)
	cls := &fakeClassifier{}

	if _, _, err := Run(ctx, s, cls, "usefulness", false, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(cls.askedFor) != len(rows) {
		t.Fatalf("the pass asked about %d notes, want %d", len(cls.askedFor), len(rows))
	}
	want := make(map[string]bool, len(rows))
	for _, m := range rows {
		want[m.Content] = true
	}
	for i, asked := range cls.askedFor {
		if !want[asked] {
			t.Errorf("note %d was sent as %q, which is not a stored note's content verbatim — "+
				"a note with no audit rows must reach the harness unchanged", i+1, asked)
		}
	}
}

// TestAUsedOrIgnoredVerdictChangesNothingResolveSendsTheHarness is the test the
// #284 popularity loop is closed by. `used` is the verdict that looks like a
// ranking signal, `ignored` is the same number with a worse name, and the moment
// either is in front of a classifier a memory that was merely retrieved — never
// consulted, let alone useful — starts to look better than one that was.
//
// So the assertion is byte equality across the whole batch, not the absence of
// one word: a reader that leaked `used` as a count, a ratio, a mention in the
// most-recent clause or a trailing byte all fail it.
func TestAUsedOrIgnoredVerdictChangesNothingResolveSendsTheHarness(t *testing.T) {
	s, ctx, rows := resolveRealStore(t)

	before := &fakeClassifier{}
	if _, _, err := Run(ctx, s, before, "usefulness", false, nil); err != nil {
		t.Fatalf("Run (no audit): %v", err)
	}

	// Plant every POSITIVE bucket over every note, twice over, in one pass each.
	// The verdicts are unattributed (rowid 0), which the writer accepts: a
	// verdict about a session rather than about one call is still a verdict.
	for _, outcome := range []string{memory.VerdictOutcomeUsed, memory.VerdictOutcomeIgnored} {
		var planted []memory.RetrievalAuditRow
		for _, m := range rows {
			planted = append(planted, memory.RetrievalAuditRow{
				ProjectID: "usefulness", SessionID: "ses_loud", Source: "search",
				MemoryID: m.ID, Outcome: outcome,
			})
		}
		refused, err := s.RecordRetrievalAudits(ctx, planted)
		if err != nil {
			t.Fatalf("RecordRetrievalAudits(%s): %v", outcome, err)
		}
		if len(refused) != 0 {
			t.Fatalf("the writer refused %d %s verdicts, so the test would prove nothing: %+v",
				len(refused), outcome, refused)
		}
	}

	after := &fakeClassifier{}
	if _, _, err := Run(ctx, s, after, "usefulness", false, nil); err != nil {
		t.Fatalf("Run (audited): %v", err)
	}
	if len(before.askedFor) != len(after.askedFor) {
		t.Fatalf("the pass asked about %d notes before and %d after the audit; a verdict "+
			"changed which notes reach the harness", len(before.askedFor), len(after.askedFor))
	}
	for i := range before.askedFor {
		if before.askedFor[i] != after.askedFor[i] {
			t.Errorf("note %d reached the harness as %q with `used`/`ignored` verdicts on file and "+
				"%q without them; a positive verdict must never reach a prompt, a ranking or a score:\n"+
				"  before: %q\n  after:  %q",
				i+1, after.askedFor[i], before.askedFor[i], before.askedFor[i], after.askedFor[i])
		}
	}
}

// TestANegativeVerdictReachesTheHarnessAsData: the feature itself. The note's
// own content is unchanged and a fixed-format line naming the negative verdicts
// follows it, and the line is inside the text the classifier is asked about
// rather than beside it — so it is read under the rubric's own «...» data rule
// with everything else in the note.
func TestANegativeVerdictReachesTheHarnessAsData(t *testing.T) {
	contradicted := "postmortem concluded: the deploy failure was a stale hash"
	store := &fakeStore{candidates: []memory.Memory{
		{ID: "M1", Content: contradicted},
		{ID: "M2", Content: "kill experiment: the graph bonus is gone, PR #210 landed it"},
	}, usefulness: map[string]memory.UsefulnessEvidence{
		"M1": {Contradicted: 2, SupersededInSession: 1, LastSession: "ses_2", LastAt: "2026-09-24 10:00:00"},
	}}
	cls := &fakeClassifier{}

	if _, _, err := Run(context.Background(), store, cls, "p1", false, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if store.usefulnessCalls != 1 {
		t.Errorf("the pass read the evidence %d times; one read covers the whole pass and a "+
			"per-memory query does not", store.usefulnessCalls)
	}

	line := (memory.UsefulnessEvidence{
		Contradicted: 2, SupersededInSession: 1, LastSession: "ses_2", LastAt: "2026-09-24 10:00:00",
	}).Line()
	want := contradicted + "\n" + line
	if !sentVerbatim(cls.askedFor, want) {
		t.Errorf("no note was sent as %q; the pass sent %q", want, cls.askedFor)
	}
	// And the note with no evidence is untouched, in the SAME pass — so the two
	// are not "the pass changed" and "the pass did not change" but one rendering
	// that is conditional on the evidence and not on the pass.
	if !sentVerbatim(cls.askedFor, "kill experiment: the graph bonus is gone, PR #210 landed it") {
		t.Errorf("the note with no audit rows was altered: the pass sent %q", cls.askedFor)
	}
}

// TestTheEvidenceLineIsCarriedInsideTheDataDelimiters: where resolve puts it is
// the difference between an annotation the rubric's own rule covers and a line of
// instructions the model may read as such. It travels with the note's content,
// inside the «...» quoteData renders.
func TestTheEvidenceLineIsCarriedInsideTheDataDelimiters(t *testing.T) {
	store := &fakeStore{candidates: []memory.Memory{
		{ID: "M1", Content: "postmortem concluded: the deploy failure was a stale hash"},
	}, usefulness: map[string]memory.UsefulnessEvidence{
		"M1": {Contradicted: 1, LastSession: "ses_2", LastAt: "2026-09-24 10:00:00"},
	}}
	cls := &fakeClassifier{}
	if _, _, err := Run(context.Background(), store, cls, "p1", false, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var seen string
	for _, c := range cls.askedFor {
		if strings.Contains(c, "postmortem") {
			seen = c
		}
	}
	if seen == "" {
		t.Fatalf("the pass sent no postmortem note: %q", cls.askedFor)
	}
	// The classifier is the seam that owns the delimiters, so assert the text it
	// was handed still IS the note plus one line — and that the single-note
	// prompt quotes the whole of it.
	quoted := quoteData(seen)
	if !strings.HasPrefix(quoted, "«postmortem concluded: the deploy failure was a stale hash\naudit: ") {
		t.Errorf("the evidence is not inside the note's data block:\n%s", quoted)
	}
	if !strings.HasSuffix(quoted, "»") {
		t.Errorf("the note's data block does not close after the evidence line:\n%s", quoted)
	}
}

// TestResolveAsksAboutTheEvidenceOncePerPass: the N+1 this slice must not ship.
// The count is over a pass with several candidates, because one read for one note
// is indistinguishable from a read per note.
func TestResolveAsksAboutTheEvidenceOncePerPass(t *testing.T) {
	var cands []memory.Memory
	for _, m := range []string{
		"kill experiment: the graph bonus is gone",
		"postmortem concluded: the deploy failure was a stale hash",
		"cost estimate from May: $148 per month projected",
		"changelog: connection leak fixed in v0.9.3",
		"root cause was a stale hash, mitigated",
	} {
		cands = append(cands, memory.Memory{ID: m[:8], Content: m})
	}
	store := &fakeStore{candidates: cands}

	if _, _, err := Run(context.Background(), store, &fakeClassifier{}, "p1", false, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if store.usefulnessCalls != 1 {
		t.Errorf("a pass over %d candidates read the evidence %d times, want 1: the evidence is "+
			"one bounded read per pass, not one per memory", len(cands), store.usefulnessCalls)
	}
}

// TestEvidenceDoesNotChangeWhatResolveMayDo: what resolve may DO is the ordinary
// pass's whole contract, and "only what they are told" is a claim about it. A
// note carrying negative evidence still comes back KEEP when the classifier says
// KEEP — the evidence is an input to a judgement, never a verdict, and never a
// demotion of resolve's own.
func TestEvidenceDoesNotChangeWhatResolveMayDo(t *testing.T) {
	keep := "kill experiment: the graph bonus is gone, PR #210 landed it"
	drop := "postmortem concluded: the deploy failure was a stale hash"
	evidence := map[string]memory.UsefulnessEvidence{
		"keep": {Contradicted: 3, LastSession: "ses_9", LastAt: "2026-09-24 10:00:00"},
		"drop": {Contradicted: 1, LastSession: "ses_9", LastAt: "2026-09-24 10:00:00"},
	}

	// The classifier keeps the note the evidence says was contradicted three times.
	store := &fakeStore{candidates: []memory.Memory{
		{ID: "keep", Content: keep}, {ID: "drop", Content: drop},
	}, usefulness: evidence}
	res, confirmed, err := Run(context.Background(), store, &fakeClassifier{}, "p1", false, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, m := range confirmed {
		if m.ID == "keep" {
			t.Errorf("resolve resolved a note the classifier kept, on the strength of audit evidence " +
				"alone; the evidence is an input to the classifier, not a verdict of resolve's own")
		}
	}
	if res.Confirmed != 0 {
		t.Errorf("Confirmed = %d, want 0 — nothing resolved without a classifier RESOLVED verdict", res.Confirmed)
	}

	// And the same evidence changes nothing when the classifier DOES resolve it.
	store2 := &fakeStore{
		candidates: []memory.Memory{{ID: "keep", Content: keep}, {ID: "drop", Content: drop}},
		usefulness: evidence,
	}
	// fakeClassifier keys off exactly what it was asked, so the drop is keyed on
	// the text the pass will actually send — the note plus its evidence line.
	asked := drop + "\n" + evidence["drop"].Line()
	cls := &fakeClassifier{drop: map[string]bool{asked: true}}
	if _, _, err := Run(context.Background(), store2, cls, "p1", true, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(store2.resolved) != 1 || store2.resolved[0] != "drop" {
		t.Errorf("resolved %v, want [drop]: a RESOLVED verdict must still be honoured with evidence in the input",
			store2.resolved)
	}
}

// TestResolveFailsOpenWhenTheEvidenceCannotBeRead: the evidence is an addition to
// a judgement that already works without it, so a store that cannot answer the
// read costs the pass nothing but the annotation. Failing the pass instead would
// turn a missing audit into a missing maintenance phase — the one outcome that
// makes the audit a dependency rather than an input.
func TestResolveFailsOpenWhenTheEvidenceCannotBeRead(t *testing.T) {
	const note = "kill experiment: the graph bonus is gone, PR #210 landed it"
	store := &fakeStore{
		candidates:    []memory.Memory{{ID: "M1", Content: note}},
		usefulnessErr: errEvidenceUnavailable,
	}
	cls := &fakeClassifier{}

	res, _, err := Run(context.Background(), store, cls, "p1", false, nil)
	if err != nil {
		t.Fatalf("Run returned %v on an unreadable evidence map; the pass must continue without it", err)
	}
	if res.Candidates != 1 {
		t.Errorf("Candidates = %d, want 1", res.Candidates)
	}
	if len(cls.askedFor) != 1 || cls.askedFor[0] != note {
		t.Errorf("the pass sent %q, want the note's content verbatim", cls.askedFor)
	}
}

// sentVerbatim reports whether the pass handed the harness exactly this text,
// rather than merely text containing it — the difference between "the evidence
// line followed the note" and "something about the note came through".
func sentVerbatim(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
