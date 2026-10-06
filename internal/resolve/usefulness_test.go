package resolve

// #648 slice 1, #880: resolve receives the audit's NEGATIVE evidence as INPUT.
//
// The evidence is a fact about a memory's past, told to the classifier beside the
// note it is about — and nothing else about resolve changes. What may be
// resolved, what the veto settles and what the pass writes are all as they were;
// the only difference is one extra line inside the text the harness is asked
// about.
//
// What #880 changed is WHERE the evidence is read, and therefore what the cache
// covers. The read now runs ABOVE the KEEP-cache gate, so a cached KEEP whose
// content the audit has since doubted is put back in the pending set — re-asked
// once, with the contradiction attached — and the stamp it is re-cached under
// (resolve.KeepStamp) covers the evidence it was judged with. A cached KEEP with
// no negative verdict is skipped exactly as before, byte for byte, so a
// converged corpus still makes no classifier call: the stamp with no evidence IS
// the content hash.
//
// The properties these tests hold are the ones that can rot silently: a
// `used` verdict must be invisible (it is the #284 popularity loop the moment a
// reader puts it in a prompt), a memory with no audit rows must produce
// byte-for-byte the input it produced before any of this existed, and the
// converged pass must stay free of classifier calls while still paying the ONE
// bounded read that lets a later contradiction reach the classifier at all.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// TestACachedKeepWithNoEvidenceIsSkippedAfterOneBoundedRead: the converged pass,
// which is the state the cache exists to make free, and the one #880 said was
// paying nothing for a contradiction nobody could show it. It still makes no
// classifier call — but it now costs ONE bounded evidence read, because that
// read is the only thing that can tell the pass whether a verdict has been
// recorded since the KEEP was cached. The gate cannot know without asking.
//
// So the bound on the new cost is "a pass that has at least one candidate left
// to decide", not "a pass that asks the classifier": a converged project pays
// the read and nothing else, and a project with no candidates at all pays
// neither (TestTheEvidenceReadIsBoundToCandidatesThatReachTheGate holds that
// side).
func TestACachedKeepWithNoEvidenceIsSkippedAfterOneBoundedRead(t *testing.T) {
	const content = "changelog: connection leak fixed in v0.9.3"
	store := &fakeStore{
		candidates: []memory.Memory{{ID: "kept", Content: content}},
		kept:       map[string]string{"kept": ContentHash(content)},
	}
	cls := &fakeClassifier{}

	res, confirmed, err := Run(context.Background(), store, cls, "p1", false, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if store.usefulnessCalls != 1 {
		t.Errorf("the converged pass read the audit evidence %d times, want 1: without that read a "+
			"contradiction recorded after the KEEP can never reach the classifier (#880)",
			store.usefulnessCalls)
	}
	if cls.calls != 0 {
		t.Errorf("classifier calls = %d, want 0 — a cached KEEP with no evidence is still skipped", cls.calls)
	}
	if res.Skipped != 1 {
		t.Errorf("res.Skipped = %d, want 1", res.Skipped)
	}
	if len(confirmed) != 0 {
		t.Errorf("confirmed = %v, want nothing (cached KEEP stays KEEP)", confirmed)
	}
}

// TestACachedKeepWithANegativeVerdictIsReaskedOnceWithTheEvidence: the defect
// #880 opened with. A plain, pre-#880 cache entry for content the audit has
// since contradicted must reach the classifier again — with the contradiction
// attached, as data inside the note — and the KEEP it comes back with must be
// re-stamped so it covers that evidence. One re-ask, then quiet: the stamp is
// built from the evidence, so the next pass's gate matches and skips.
func TestACachedKeepWithANegativeVerdictIsReaskedOnceWithTheEvidence(t *testing.T) {
	const content = "postmortem concluded: the deploy failure was a stale hash"
	evidence := map[string]memory.UsefulnessEvidence{
		"M1": {Contradicted: 2, LastSession: "ses_2", LastAt: "2026-09-24 10:00:00"},
	}
	store := &fakeStore{
		candidates: []memory.Memory{{ID: "M1", Content: content}},
		kept:       map[string]string{"M1": ContentHash(content)}, // a plain, pre-#880 cache entry
		usefulness: evidence,
	}

	// Pass 1: the cached KEEP is re-asked exactly once, and the evidence rides
	// the note verbatim rather than beside it.
	cls := &fakeClassifier{}
	res, _, err := Run(context.Background(), store, cls, "p1", false, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if cls.calls != 1 {
		t.Fatalf("classifier calls = %d, want 1 — a cached KEEP the audit has contradicted must be re-asked",
			cls.calls)
	}
	if res.Skipped != 0 {
		t.Errorf("res.Skipped = %d, want 0: the re-asked note is not a cache hit", res.Skipped)
	}
	want := content + "\n" + evidence["M1"].Line()
	if !sentVerbatim(cls.askedFor, want) {
		t.Errorf("the pass sent %q, want the note plus its evidence line %q", cls.askedFor, want)
	}
	if store.usefulnessCalls != 1 {
		t.Errorf("usefulnessCalls = %d, want 1 (one bounded read per pass)", store.usefulnessCalls)
	}

	// Pass 2 (apply): the classifier keeps it again, and the re-stamp covers the
	// evidence the KEEP was judged with — which is what makes pass 3 quiet.
	cls2 := &fakeClassifier{}
	if _, _, err := Run(context.Background(), store, cls2, "p1", true, nil); err != nil {
		t.Fatalf("Run apply: %v", err)
	}
	if cls2.calls != 1 {
		t.Errorf("apply re-asked %d times, want 1", cls2.calls)
	}
	if len(store.markedKept) != 1 {
		t.Fatalf("markedKept = %v, want one KEEP stamp", store.markedKept)
	}
	stamp := store.markedKept[0]["M1"]
	if stamp != KeepStamp(content, evidence["M1"]) {
		t.Errorf("re-stamped as %q, want the stamp covering that evidence %q", stamp, KeepStamp(content, evidence["M1"]))
	}
	if stamp == ContentHash(content) {
		t.Errorf("the re-stamp is the bare content hash %q: a KEEP judged WITH evidence must not be cached as "+
			"one judged without it, or the next pass re-asks forever", stamp)
	}

	// Pass 3: quiet — one re-ask per new evidence, not one per pass.
	cls3 := &fakeClassifier{}
	res, _, err = Run(context.Background(), store, cls3, "p1", false, nil)
	if err != nil {
		t.Fatalf("Run after re-stamp: %v", err)
	}
	if cls3.calls != 0 {
		t.Errorf("classifier calls after the re-stamp = %d, want 0 — the stamp already covers this evidence",
			cls3.calls)
	}
	if res.Skipped != 1 {
		t.Errorf("res.Skipped = %d, want 1", res.Skipped)
	}
	if store.usefulnessCalls != 3 {
		t.Errorf("usefulnessCalls = %d, want 3 — the read is per PASS, never per memory or per re-ask",
			store.usefulnessCalls)
	}
}

// TestANewerNegativeVerdictReasksTheCachedKeepAgain: the stamp moves when the
// audit adds a verdict, and only then. A cache entry already stamped over the
// evidence it was judged with is a hit; a second contradiction changes the
// fingerprint, so the same note is re-asked once more with the new line and
// re-stamped — and quiet again. This is the whole "re-ask ONCE per new
// evidence" bargain, in both directions.
func TestANewerNegativeVerdictReasksTheCachedKeepAgain(t *testing.T) {
	const content = "cost estimate from May: $148 per month projected"
	first := memory.UsefulnessEvidence{Contradicted: 1, LastAt: "2026-09-24 10:00:00"}
	store := &fakeStore{
		candidates: []memory.Memory{{ID: "M1", Content: content}},
		kept:       map[string]string{"M1": KeepStamp(content, first)},
		usefulness: map[string]memory.UsefulnessEvidence{"M1": first},
	}

	cls := &fakeClassifier{}
	res, _, err := Run(context.Background(), store, cls, "p1", false, nil)
	if err != nil {
		t.Fatalf("Run (stamp covers its own evidence): %v", err)
	}
	if cls.calls != 0 || res.Skipped != 1 {
		t.Fatalf("calls=%d Skipped=%d, want 0/1 — a stamp covering the evidence it was judged with is a cache hit",
			cls.calls, res.Skipped)
	}

	// The audit records a second negative verdict for the same memory.
	second := memory.UsefulnessEvidence{Contradicted: 2, LastAt: "2026-09-25 11:00:00"}
	store.usefulness = map[string]memory.UsefulnessEvidence{"M1": second}

	cls2 := &fakeClassifier{}
	res, _, err = Run(context.Background(), store, cls2, "p1", false, nil)
	if err != nil {
		t.Fatalf("Run (newer verdict): %v", err)
	}
	if cls2.calls != 1 || res.Skipped != 0 {
		t.Fatalf("calls=%d Skipped=%d, want 1/0 — a newer verdict must move the stamp and re-ask once",
			cls2.calls, res.Skipped)
	}
	if want := content + "\n" + second.Line(); !sentVerbatim(cls2.askedFor, want) {
		t.Errorf("sent %q, want %q — the re-ask carries the CURRENT evidence, not the first verdict's",
			cls2.askedFor, want)
	}

	// Re-stamped over the new evidence, the pass is quiet again.
	cls3 := &fakeClassifier{}
	if _, _, err := Run(context.Background(), store, cls3, "p1", true, nil); err != nil {
		t.Fatalf("Run apply: %v", err)
	}
	if store.kept["M1"] != KeepStamp(content, second) {
		t.Errorf("stored stamp = %q, want %q", store.kept["M1"], KeepStamp(content, second))
	}
	cls4 := &fakeClassifier{}
	res, _, err = Run(context.Background(), store, cls4, "p1", false, nil)
	if err != nil {
		t.Fatalf("Run after re-stamp: %v", err)
	}
	if cls4.calls != 0 || res.Skipped != 1 {
		t.Errorf("calls=%d Skipped=%d, want 0/1 — the second stamp must be as quiet as the first",
			cls4.calls, res.Skipped)
	}
}

// TestAPositiveVerdictNeverReasksACachedKeep: the other half of the bargain,
// against a REAL store. The reader returns only the two negative buckets, so
// `used` and `ignored` can never move a stamp — and a memory carrying only
// positive verdicts is skipped exactly as it was before any of this existed.
// It runs green without the fix as well: this is the preservation guard for the
// cache's own purpose, not the defect's test.
func TestAPositiveVerdictNeverReasksACachedKeep(t *testing.T) {
	s, ctx, rows := resolveRealStore(t)
	hashes := make(map[string]string, len(rows))
	for _, m := range rows {
		hashes[m.ID] = ContentHash(m.Content)
	}
	if err := s.MarkResolveKept(ctx, "usefulness", hashes); err != nil {
		t.Fatalf("MarkResolveKept: %v", err)
	}
	for _, outcome := range []string{memory.VerdictOutcomeUsed, memory.VerdictOutcomeIgnored} {
		var planted []memory.RetrievalAuditRow
		for _, m := range rows {
			planted = append(planted, memory.RetrievalAuditRow{
				ProjectID: "usefulness", SessionID: "ses_loud", Source: "search",
				MemoryID: m.ID, Outcome: outcome,
			})
		}
		if refused, err := s.RecordRetrievalAudits(ctx, planted); err != nil {
			t.Fatalf("RecordRetrievalAudits(%s): %v", outcome, err)
		} else if len(refused) != 0 {
			t.Fatalf("the writer refused %d %s verdicts: %+v", len(refused), outcome, refused)
		}
	}

	cls := &fakeClassifier{}
	res, _, err := Run(ctx, s, cls, "usefulness", false, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if cls.calls != 0 {
		t.Errorf("classifier calls = %d, want 0 — a positive verdict is not a reason to spend a call", cls.calls)
	}
	if res.Skipped != len(rows) {
		t.Errorf("res.Skipped = %d, want %d", res.Skipped, len(rows))
	}
	stored, err := s.ResolveKeptHashes(ctx, "usefulness")
	if err != nil {
		t.Fatalf("ResolveKeptHashes: %v", err)
	}
	for _, m := range rows {
		if stored[m.ID] != ContentHash(m.Content) {
			t.Errorf("stamp for %s = %q, want the plain content hash %q: positive verdicts are filtered in "+
				"SQL and must not appear in a stamp at all", m.ID, stored[m.ID], ContentHash(m.Content))
		}
	}
}

// TestKeepStampWithNoEvidenceIsTheContentHashByteForByte: the property every
// existing cache entry depends on. With no negative verdict the stamp IS the
// content hash — byte for byte — so no `keepCacheHashVersion` bump is needed
// and every hash already on disk stays valid; and the stamp moves only when the
// evidence moves, which is what makes "re-ask once" a bounded promise rather
// than a per-pass one.
func TestKeepStampWithNoEvidenceIsTheContentHashByteForByte(t *testing.T) {
	const content = "kill experiment finding: 7.3% cross-session links, removed"
	base := ContentHash(content)

	if got := KeepStamp(content, memory.UsefulnessEvidence{}); got != base {
		t.Errorf("KeepStamp with no evidence = %q, want the plain content hash %q — every hash already on disk "+
			"is one this function must still produce", got, base)
	}
	// A positive-only history cannot even be expressed: UsefulnessEvidence
	// carries the two negative buckets and nothing else, so there is no value of
	// the type that could move a stamp for being popular.
	ev := memory.UsefulnessEvidence{Contradicted: 1, LastSession: "ses_9", LastAt: "2026-09-24 10:00:00"}
	withEvidence := KeepStamp(content, ev)
	if withEvidence == base {
		t.Errorf("a contradicted note stamped %q, the same as an unjudged one: the stamp must move when the "+
			"audit has doubted the content", withEvidence)
	}
	if got := KeepStamp(content, ev); got != withEvidence {
		t.Errorf("KeepStamp is not deterministic: %q then %q — a stamp that drifts re-asks every pass", got, withEvidence)
	}
	if got := KeepStamp(content, memory.UsefulnessEvidence{SupersededInSession: 1, LastAt: "2026-09-24 10:00:00"}); got == base {
		t.Errorf("a superseded_in_session verdict left the stamp at the plain hash: the second negative bucket " +
			"must move it too")
	}
	// Same counts, newer verdict: the fingerprint moves, so a NEW verdict on a
	// memory already stamped re-asks it once.
	newer := KeepStamp(content, memory.UsefulnessEvidence{Contradicted: 1, LastAt: "2026-09-25 11:00:00"})
	if newer == withEvidence {
		t.Errorf("a newer verdict left the stamp at %q: nothing would ever re-ask a note the audit has just "+
			"contradicted again", newer)
	}
	// And the stamp is about THIS content: another note's evidence cannot move it.
	if KeepStamp(content+" more", ev) == withEvidence {
		t.Errorf("two different contents share a stamp under the same evidence")
	}
}

// TestTheEvidenceReadIsBoundToCandidatesThatReachTheGate: the cost side of
// #880. The read is bounded to a pass that has something to decide — no
// candidates, or nothing but candidates the veto settles, pays neither read nor
// call — and a pass with a cache hit pays the read and no call. That is the
// whole shape of the new cost, and the first two cases are the passes #884 made
// free, which this must not re-price.
func TestTheEvidenceReadIsBoundToCandidatesThatReachTheGate(t *testing.T) {
	t.Run("no candidates", func(t *testing.T) {
		store := &fakeStore{}
		cls := &fakeClassifier{}
		if _, _, err := Run(context.Background(), store, cls, "p1", false, nil); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if store.usefulnessCalls != 0 {
			t.Errorf("usefulnessCalls = %d, want 0: a pass with no candidate reaches no gate", store.usefulnessCalls)
		}
		if cls.calls != 0 {
			t.Errorf("classifier calls = %d, want 0", cls.calls)
		}
	})
	t.Run("every candidate vetoed", func(t *testing.T) {
		store := &fakeStore{candidates: []memory.Memory{{
			ID: "vetoed", Content: "kill experiment: never run the restore with both ends on one spindle",
		}}}
		cls := &fakeClassifier{}
		res, _, err := Run(context.Background(), store, cls, "p1", false, nil)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res.Vetoed != 1 {
			t.Fatalf("res.Vetoed = %d, want 1 — the veto must be what settles this note", res.Vetoed)
		}
		if store.usefulnessCalls != 0 {
			t.Errorf("usefulnessCalls = %d, want 0: a note the veto settles reaches no cache gate",
				store.usefulnessCalls)
		}
		if cls.calls != 0 {
			t.Errorf("classifier calls = %d, want 0", cls.calls)
		}
	})
	t.Run("one cached candidate", func(t *testing.T) {
		const content = "changelog: connection leak fixed in v0.9.3"
		store := &fakeStore{
			candidates: []memory.Memory{{ID: "kept", Content: content}},
			kept:       map[string]string{"kept": ContentHash(content)},
		}
		cls := &fakeClassifier{}
		if _, _, err := Run(context.Background(), store, cls, "p1", false, nil); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if store.usefulnessCalls != 1 {
			t.Errorf("usefulnessCalls = %d, want 1: a candidate at the gate is the one payer of this read",
				store.usefulnessCalls)
		}
		if cls.calls != 0 {
			t.Errorf("classifier calls = %d, want 0", cls.calls)
		}
	})
}

// TestAConvergedCorpusReasksOnceWhenTheAuditContradictsIt: the whole #880 flow
// against a REAL store, through the real reader and the real cache writer —
// converge, stay quiet, take one contradiction, re-ask once with it, go quiet,
// then take a second contradiction and do it again. Every pass in between with
// nothing new to say must make no classifier call at all.
func TestAConvergedCorpusReasksOnceWhenTheAuditContradictsIt(t *testing.T) {
	s, ctx, rows := resolveRealStore(t)
	const project = "usefulness"
	contradicted := rows[0]

	// Pass 1: three notes, no evidence yet, three explicit KEEPs cached.
	cls1 := &fakeClassifier{}
	if _, _, err := Run(ctx, s, cls1, project, true, nil); err != nil {
		t.Fatalf("Run (converge): %v", err)
	}
	if cls1.calls != 1 || len(cls1.askedFor) != len(rows) {
		t.Fatalf("first pass calls=%d asked=%d, want 1 call over %d notes", cls1.calls, len(cls1.askedFor), len(rows))
	}
	// Pass 2: converged — no calls.
	cls2 := &fakeClassifier{}
	res, _, err := Run(ctx, s, cls2, project, false, nil)
	if err != nil {
		t.Fatalf("Run (converged): %v", err)
	}
	if cls2.calls != 0 || res.Skipped != len(rows) {
		t.Fatalf("converged pass calls=%d Skipped=%d, want 0/%d", cls2.calls, res.Skipped, len(rows))
	}

	// The audit contradicts ONE of the three notes.
	plantVerdict(t, ctx, s, project, contradicted.ID, memory.VerdictOutcomeContradicted)
	evidence := mustEvidence(t, ctx, s, project)
	first, ok := evidence[contradicted.ID]
	if !ok || first.Line() == "" {
		t.Fatalf("the planted contradiction is not in the reader's answer: %+v", evidence)
	}

	// Pass 3: exactly one re-ask, carrying the evidence, and the other two notes
	// are still skipped.
	cls3 := &fakeClassifier{}
	res, _, err = Run(ctx, s, cls3, project, false, nil)
	if err != nil {
		t.Fatalf("Run (contradicted): %v", err)
	}
	if cls3.calls != 1 || len(cls3.askedFor) != 1 {
		t.Fatalf("calls=%d asked=%d, want one re-ask of the one contradicted note", cls3.calls, len(cls3.askedFor))
	}
	if want := contradicted.Content + "\n" + first.Line(); !sentVerbatim(cls3.askedFor, want) {
		t.Errorf("sent %q, want %q", cls3.askedFor, want)
	}
	if res.Skipped != len(rows)-1 {
		t.Errorf("res.Skipped = %d, want %d", res.Skipped, len(rows)-1)
	}

	// Pass 4 (apply): KEEP again, re-stamped over that evidence.
	cls4 := &fakeClassifier{}
	if _, _, err := Run(ctx, s, cls4, project, true, nil); err != nil {
		t.Fatalf("Run apply: %v", err)
	}
	if cls4.calls != 1 {
		t.Errorf("apply calls = %d, want 1", cls4.calls)
	}
	stored, err := s.ResolveKeptHashes(ctx, project)
	if err != nil {
		t.Fatalf("ResolveKeptHashes: %v", err)
	}
	if got, want := stored[contradicted.ID], KeepStamp(contradicted.Content, first); got != want {
		t.Errorf("stored stamp = %q, want %q", got, want)
	}
	// Pass 5: quiet again, and the two untouched notes were never re-stamped to
	// anything but their own content hash.
	cls5 := &fakeClassifier{}
	res, _, err = Run(ctx, s, cls5, project, false, nil)
	if err != nil {
		t.Fatalf("Run (quiet): %v", err)
	}
	if cls5.calls != 0 || res.Skipped != len(rows) {
		t.Fatalf("calls=%d Skipped=%d, want 0/%d — the re-stamp must make the pass quiet", cls5.calls, res.Skipped, len(rows))
	}
	for _, m := range rows[1:] {
		if stored[m.ID] != ContentHash(m.Content) {
			t.Errorf("stamp for the untouched note %s = %q, want its plain content hash", m.ID, stored[m.ID])
		}
	}

	// A SECOND contradiction on the same note: one more re-ask, once more.
	plantVerdict(t, ctx, s, project, contradicted.ID, memory.VerdictOutcomeContradicted)
	evidence = mustEvidence(t, ctx, s, project)
	second := evidence[contradicted.ID]
	if second.Contradicted != 2 {
		t.Fatalf("Contradicted = %d, want 2", second.Contradicted)
	}
	cls6 := &fakeClassifier{}
	if _, _, err := Run(ctx, s, cls6, project, false, nil); err != nil {
		t.Fatalf("Run (second contradiction): %v", err)
	}
	if cls6.calls != 1 || len(cls6.askedFor) != 1 {
		t.Fatalf("calls=%d asked=%d, want one re-ask", cls6.calls, len(cls6.askedFor))
	}
	if want := contradicted.Content + "\n" + second.Line(); !sentVerbatim(cls6.askedFor, want) {
		t.Errorf("sent %q, want %q", cls6.askedFor, want)
	}
	cls7 := &fakeClassifier{}
	if _, _, err := Run(ctx, s, cls7, project, true, nil); err != nil {
		t.Fatalf("Run apply 2: %v", err)
	}
	cls8 := &fakeClassifier{}
	res, _, err = Run(ctx, s, cls8, project, false, nil)
	if err != nil {
		t.Fatalf("Run (quiet 2): %v", err)
	}
	if cls8.calls != 0 || res.Skipped != len(rows) {
		t.Errorf("calls=%d Skipped=%d, want 0/%d after the second re-stamp", cls8.calls, res.Skipped, len(rows))
	}
}

// plantVerdict records one unattributed verdict against a memory — the shape
// the audit writer accepts for a verdict about a session rather than about one
// retrieval call.
func plantVerdict(t *testing.T, ctx context.Context, s *memory.Store, project, memoryID, outcome string) {
	t.Helper()
	refused, err := s.RecordRetrievalAudits(ctx, []memory.RetrievalAuditRow{{
		ProjectID: project, SessionID: "ses_loud", Source: "search",
		MemoryID: memoryID, Outcome: outcome,
	}})
	if err != nil {
		t.Fatalf("RecordRetrievalAudits(%s): %v", outcome, err)
	}
	if len(refused) != 0 {
		t.Fatalf("the writer refused the %s verdict: %+v", outcome, refused)
	}
}

// mustEvidence is the reader the pass itself uses, so a test that asserts what
// the classifier was handed cannot drift from what the store actually says.
func mustEvidence(t *testing.T, ctx context.Context, s *memory.Store, project string) map[string]memory.UsefulnessEvidence {
	t.Helper()
	evidence, err := s.UsefulnessByMemory(ctx, project)
	if err != nil {
		t.Fatalf("UsefulnessByMemory: %v", err)
	}
	return evidence
}

// TestMeasuredCostOfTheConvergedPassEvidenceRead is the number #880's fix has
// to justify: the one thing a converged pass pays that it did not pay before is
// ONE bounded evidence read, and this measures it on a real store rather than
// estimating it. The corpus is stated with the number so the figure can be
// read against a store of a different size.
//
// It prints unconditionally (fmt.Printf, no -v needed): the measurement is the
// deliverable of the run, not a log line.
func TestMeasuredCostOfTheConvergedPassEvidenceRead(t *testing.T) {
	const (
		memories   = 1000
		negatives  = 100 // one in ten also carries a contradicted verdict
		sampleRuns = 5
		project    = "measure"
	)

	ctx := context.Background()
	db, err := memory.OpenDB(filepath.Join(t.TempDir(), "measure.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := memory.NewStore(db, nil)
	if err := s.EnsureProject(ctx, project, "/tmp/measure", project); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	ids := make([]string, 0, memories)
	contents := make([]string, 0, memories)
	for i := 0; i < memories; i++ {
		content := fmt.Sprintf("changelog: entry %d completed and shipped", i)
		id, err := s.Create(ctx, project, memory.Memory{
			Category: "fact", Content: content, Importance: 0.5, Source: "mcp",
		})
		if err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		ids = append(ids, id)
		contents = append(contents, content)
	}

	// The audit shape a converged project actually has: one `used` row per
	// memory (every memory a call retrieved), plus a handful of contradictions.
	const verdictBatch = 500
	for start := 0; start < memories; start += verdictBatch {
		end := min(start+verdictBatch, memories)
		var rows []memory.RetrievalAuditRow
		for i := start; i < end; i++ {
			rows = append(rows, memory.RetrievalAuditRow{
				ProjectID: project, SessionID: "ses_m", Source: "search",
				MemoryID: ids[i], Outcome: memory.VerdictOutcomeUsed,
			})
		}
		if refused, err := s.RecordRetrievalAudits(ctx, rows); err != nil || len(refused) != 0 {
			t.Fatalf("plant used verdicts: %v (%d refused)", err, len(refused))
		}
	}
	var negativesPlanted []memory.RetrievalAuditRow
	for i := 0; i < negatives; i++ {
		negativesPlanted = append(negativesPlanted, memory.RetrievalAuditRow{
			ProjectID: project, SessionID: "ses_m", Source: "search",
			MemoryID: ids[i], Outcome: memory.VerdictOutcomeContradicted,
		})
	}
	if refused, err := s.RecordRetrievalAudits(ctx, negativesPlanted); err != nil || len(refused) != 0 {
		t.Fatalf("plant contradicted verdicts: %v (%d refused)", err, len(refused))
	}

	// Converge: every memory carries a plain, no-evidence KEEP stamp, which is
	// the state a first pass leaves behind.
	stamps := make(map[string]string, memories)
	for i, id := range ids {
		stamps[id] = ContentHash(contents[i])
	}
	if err := s.MarkResolveKept(ctx, project, stamps); err != nil {
		t.Fatalf("MarkResolveKept: %v", err)
	}

	// The extra read itself: what a converged pass did not do at all before
	// #880, and does once per pass after it.
	var readBest, readTotal time.Duration
	var evidenceSeen int
	for i := 0; i < sampleRuns; i++ {
		start := time.Now()
		evidence, err := s.UsefulnessByMemory(ctx, project)
		if err != nil {
			t.Fatalf("UsefulnessByMemory: %v", err)
		}
		elapsed := time.Since(start)
		evidenceSeen = len(evidence)
		if i == 0 || elapsed < readBest {
			readBest = elapsed
		}
		readTotal += elapsed
	}
	if evidenceSeen != negatives {
		t.Fatalf("the reader returned evidence for %d memories, want the %d planted", evidenceSeen, negatives)
	}

	// And the whole converged pass, read included, applied, against a
	// classifier: the contradicted notes are re-asked ONCE (that is the fix),
	// stamped with their evidence, and then quiet — so the count that matters
	// is the first pass's calls against the ones that follow.
	var passBest time.Duration
	var firstPassCalls, laterPassCalls int
	for i := 0; i < sampleRuns; i++ {
		cls := &fakeClassifier{}
		start := time.Now()
		if _, _, err := Run(ctx, s, cls, project, true, nil); err != nil {
			t.Fatalf("Run: %v", err)
		}
		elapsed := time.Since(start)
		if i == 0 {
			firstPassCalls = cls.calls
		} else {
			laterPassCalls += cls.calls
		}
		if i == 0 || elapsed < passBest {
			passBest = elapsed
		}
	}

	fmt.Printf("#880 measured cost (real store, %d memories, %d audit rows: %d used + %d contradicted, "+
		"%d with evidence): UsefulnessByMemory — best %s, mean %s over %d runs; full applied Run incl. that "+
		"read — best %s over %d runs (first pass: %d IsResolvedBatch call(s) carrying the %d re-asked notes — "+
		"the real harness chunks those into <=8-note calls — then %d call(s) across the %d quieter passes that follow)\n",
		memories, memories+negatives, memories, negatives, evidenceSeen,
		readBest.Round(time.Microsecond), (readTotal / sampleRuns).Round(time.Microsecond), sampleRuns,
		passBest.Round(time.Microsecond), sampleRuns, firstPassCalls, negatives,
		laterPassCalls, sampleRuns-1)
}
