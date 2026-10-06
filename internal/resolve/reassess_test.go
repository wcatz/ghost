package resolve

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestReassessRepairsWrongResolutions is the repair path from issue #640: the
// live database already holds the wrong resolutions, so --reassess re-runs the
// vetoes and the classifier over ALREADY-resolved memories and returns the ones
// that come back KEEP. Dry-run writes nothing and still prints the list.
func TestReassessRepairsWrongResolutions(t *testing.T) {
	vetoed := memory.Memory{ID: "vetoed", Category: "gotcha",
		Content: "NEVER run `dingo database restore` with source and target on the same spindle"}
	// Deliberately not vetoed: no imperative and no open marker, so this one is
	// the classifier's call to get right (and its reason-less RESOLVED is the
	// shape #640 measured).
	kept := memory.Memory{ID: "kept", Category: "gotcha",
		Content: "Fixed (PR #240): the CI job leaked the pull-request token in its log output."}
	stillResolved := memory.Memory{ID: "still", Category: "changelog",
		Content: "Cost estimate from May: $148/mo projected; actuals have since replaced it."}
	store := &fakeStore{alreadyResolved: []memory.Memory{vetoed, kept, stillResolved}}
	// The harness KEEPs the still-true rule and RESOLVES the cost estimate.
	cls := &fakeClassifier{drop: map[string]bool{stillResolved.Content: true}}

	res, reKept, err := Reassess(context.Background(), store, cls, "proj", false, Scope{}, nil)
	if err != nil {
		t.Fatalf("Reassess dry: %v", err)
	}
	if len(store.cleared) != 0 {
		t.Errorf("dry run cleared %v, want nothing written", store.cleared)
	}
	if res.Vetoed != 1 || res.ReKept != 2 || res.StillResolved != 1 {
		t.Fatalf("vetoed=%d reKept=%d stillResolved=%d, want 1/2/1", res.Vetoed, res.ReKept, res.StillResolved)
	}
	if res.Loaded != 3 {
		t.Errorf("res.Loaded = %d, want 3", res.Loaded)
	}
	if len(reKept) != 2 || reKept[0].ID != "vetoed" || reKept[1].ID != "kept" {
		t.Fatalf("reKept = %v, want [vetoed kept] in load order", reKept)
	}
	if cls.calls != 1 {
		t.Errorf("classifier calls = %d, want 1 (the vetoed note is never asked about)", cls.calls)
	}

	// Apply: the two KEEP notes return to ranked injection and the harness
	// KEEP is cached so the ordinary pass does not re-ask it.
	res, _, err = Reassess(context.Background(), store, cls, "proj", true, Scope{}, nil)
	if err != nil {
		t.Fatalf("Reassess apply: %v", err)
	}
	if len(store.cleared) != 2 || store.cleared[0] != "vetoed" || store.cleared[1] != "kept" {
		t.Fatalf("cleared = %v, want [vetoed kept]", store.cleared)
	}
	if res.Cleared != 2 {
		t.Errorf("res.Cleared = %d, want 2", res.Cleared)
	}
	if store.setResolvedCalled {
		t.Error("reassess must never stamp a new resolution, only clear one")
	}
	if got := store.kept["kept"]; got != ContentHash(kept.Content) {
		t.Errorf("cached KEEP hash for %q = %q, want %q", "kept", got, ContentHash(kept.Content))
	}
	if _, ok := store.kept["vetoed"]; ok {
		t.Errorf("a vetoed note must not be KEEP-cached; cache = %v", store.kept)
	}
}

// TestReassessIgnoresKeywordPrefilter: the prefilter exists to bound the cost
// of the ordinary pass, and it is exactly the note's own resolved-sounding
// framing that let a wrong resolution through in the first place. Reassess
// re-asks every already-resolved memory so a rule with no resolution keyword
// ("the default branch is main") can come back KEEP.
func TestReassessIgnoresKeywordPrefilter(t *testing.T) {
	rule := memory.Memory{ID: "rule", Category: "gotcha",
		Content: "the ledgerstate import silently drops CalculationVersion on restore"}
	store := &fakeStore{alreadyResolved: []memory.Memory{rule}}
	cls := &fakeClassifier{drop: map[string]bool{}}

	res, reKept, err := Reassess(context.Background(), store, cls, "proj", false, Scope{}, nil)
	if err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	if cls.calls != 1 || res.ReKept != 1 || len(reKept) != 1 {
		t.Errorf("calls=%d reKept=%d list=%v, want the keywordless note re-asked and kept", cls.calls, res.ReKept, reKept)
	}
}

// TestReassessSkipsCachedKeepVerdicts: a content hash already recorded as a KEEP
// is not re-asked — the same convergence the ordinary pass has.
func TestReassessSkipsCachedKeepVerdicts(t *testing.T) {
	content := "Cost estimate from May: $148/mo projected; actuals have since replaced it."
	store := &fakeStore{
		alreadyResolved: []memory.Memory{{ID: "cached", Content: content}},
		kept:            map[string]string{"cached": ContentHash(content)},
	}
	cls := &fakeClassifier{drop: map[string]bool{}}

	res, reKept, err := Reassess(context.Background(), store, cls, "proj", true, Scope{}, nil)
	if err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	if cls.calls != 0 {
		t.Errorf("classifier calls = %d, want 0 for a cached KEEP", cls.calls)
	}
	if res.Cached != 1 {
		t.Errorf("res.Cached = %d, want 1", res.Cached)
	}
	if len(reKept) != 1 || len(store.cleared) != 1 {
		t.Errorf("reKept = %v cleared = %v, want the cached KEEP returned to injection", reKept, store.cleared)
	}
}

// TestReassessSkipsAStoredStampThatAlreadyCoversTheEvidence: the other half of
// #880 review finding 1. The repair does not only WRITE the ordinary pass's
// key, it READS it — so a row whose stored stamp already carries the evidence
// the audit holds is skipped here exactly as the ordinary gate skips it, with no
// classifier call, rather than re-asked for evidence it was already judged
// under. Reverted to comparing the bare content hash, the two passes disagree
// again: this row's stamp is not a bare hash, so the repair re-asks a note the
// audit has already had its say about.
func TestReassessSkipsAStoredStampThatAlreadyCoversTheEvidence(t *testing.T) {
	content := "kill experiment: the graph bonus is gone, PR #210 landed it"
	ev := memory.UsefulnessEvidence{Contradicted: 2, SupersededInSession: 1,
		LastSession: "ses_2", LastAt: "2026-09-24 10:00:00"}
	store := &fakeStore{
		alreadyResolved: []memory.Memory{{ID: "M1", Content: content}},
		kept:            map[string]string{"M1": KeepStamp(content, ev)},
		usefulness:      map[string]memory.UsefulnessEvidence{"M1": ev},
	}
	cls := &fakeClassifier{}

	res, reKept, err := Reassess(context.Background(), store, cls, "proj", true, Scope{}, nil)
	if err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	if res.Cached != 1 || cls.calls != 0 {
		t.Errorf("cached=%d classifier calls=%d, want 1 and 0: a stamp that already covers the evidence "+
			"is the same skip the ordinary gate gives it (%v)", res.Cached, cls.calls, cls.askedFor)
	}
	if len(reKept) != 1 || len(store.cleared) != 1 {
		t.Errorf("reKept=%v cleared=%v, want the cached row returned to injection", reKept, store.cleared)
	}
}

// TestReassessLeavesUnknownAlone: an unparseable verdict is not an implicit
// KEEP, so the note keeps its resolved_at and is offered again next pass.
func TestReassessLeavesUnknownAlone(t *testing.T) {
	content := "Cost estimate from May: $148/mo projected; actuals have since replaced it."
	store := &fakeStore{alreadyResolved: []memory.Memory{{ID: "m", Content: content}}}
	cls := &fakeClassifier{unknown: map[string]bool{content: true}}

	res, reKept, err := Reassess(context.Background(), store, cls, "proj", true, Scope{}, nil)
	if err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	if res.Unknown != 1 {
		t.Errorf("res.Unknown = %d, want 1", res.Unknown)
	}
	if len(reKept) != 0 || len(store.cleared) != 0 {
		t.Errorf("reKept = %v cleared = %v, want nothing cleared on an UNKNOWN verdict", reKept, store.cleared)
	}
	if len(store.markedKept) != 0 {
		t.Errorf("an UNKNOWN verdict must not be KEEP-cached: %v", store.markedKept)
	}

	res, _, err = Reassess(context.Background(), store, cls, "proj", true, Scope{}, nil)
	if err != nil {
		t.Fatalf("Reassess second: %v", err)
	}
	if cls.calls != 2 {
		t.Errorf("classifier calls = %d, want 2 (the unknown note is re-asked)", cls.calls)
	}
	if res.Unknown != 1 {
		t.Errorf("second pass Unknown = %d, want 1", res.Unknown)
	}
}

// TestReassessStampsTheEvidenceItJudgedTheNoteUnder: the repair pass and the
// ordinary pass must write and read ONE key (#880 review finding 1). The repair
// reads the same audit evidence the ordinary gate reads, so it can compare the
// stamp it is about to overwrite — and it asks the classifier with that
// evidence appended, because a stamp claiming a verdict was judged under
// evidence is only honest when the verdict really was.
func TestReassessStampsTheEvidenceItJudgedTheNoteUnder(t *testing.T) {
	content := "kill experiment: the graph bonus is gone, PR #210 landed it"
	evidence := map[string]memory.UsefulnessEvidence{
		"M1": {Contradicted: 2, SupersededInSession: 1, LastSession: "ses_2", LastAt: "2026-09-24 10:00:00"},
	}
	store := &fakeStore{
		alreadyResolved: []memory.Memory{{ID: "M1", Content: content}},
		usefulness:      evidence,
	}
	cls := &fakeClassifier{}

	if _, _, err := Reassess(context.Background(), store, cls, "proj", true, Scope{}, nil); err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	if got, want := store.kept["M1"], KeepStamp(content, evidence["M1"]); got != want {
		t.Errorf("reassess stamped %q, want %q: the ordinary gate compares KeepStamp over the evidence "+
			"it read, so a stamp that does not cover the evidence is a stamp the next pass misses and "+
			"a note it re-asks (#880)", got, want)
	}
	if want := content + "\n" + evidence["M1"].Line(); !sentVerbatim(cls.askedFor, want) {
		t.Errorf("reassess asked about %q, want %q: the stamp records the evidence the verdict was "+
			"judged with, so the verdict has to have been judged with it", cls.askedFor, want)
	}
}

// TestReassessReadsTheEvidenceOnceForThePool: the read is ONE bounded read per
// repair over a pool with rows in it, and nothing at all over an empty pool —
// the same bound Run holds (TestTheEvidenceReadIsBoundToCandidatesThatReachTheGate).
func TestReassessReadsTheEvidenceOnceForThePool(t *testing.T) {
	cls := &fakeClassifier{}

	empty := &fakeStore{}
	if _, _, err := Reassess(context.Background(), empty, cls, "proj", true, Scope{}, nil); err != nil {
		t.Fatalf("Reassess (empty pool): %v", err)
	}
	if empty.usefulnessCalls != 0 {
		t.Errorf("usefulnessCalls = %d, want 0: a repair with no row to judge reads no evidence",
			empty.usefulnessCalls)
	}

	store := &fakeStore{
		alreadyResolved: []memory.Memory{
			{ID: "M1", Content: "kill experiment: the graph bonus is gone, PR #210 landed it"},
			{ID: "M2", Content: "postmortem concluded: the deploy failure was a stale hash"},
		},
		usefulness: map[string]memory.UsefulnessEvidence{"M1": {Contradicted: 1, LastAt: "2026-09-24 10:00:00"}},
	}
	if _, _, err := Reassess(context.Background(), store, cls, "proj", true, Scope{}, nil); err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	if store.usefulnessCalls != 1 {
		t.Errorf("usefulnessCalls = %d, want 1 — one bounded read for the whole pool, never one per memory",
			store.usefulnessCalls)
	}
}

// TestReassessFailsOpenWhenTheEvidenceCannotBeRead: an unreadable audit must
// not fail a repair, and it must not let the repair claim evidence it never
// read — the stamp it writes falls back to the bare content hash, which the
// ordinary pass re-asks (errs toward asking) rather than trusts.
func TestReassessFailsOpenWhenTheEvidenceCannotBeRead(t *testing.T) {
	content := "kill experiment: the graph bonus is gone, PR #210 landed it"
	store := &fakeStore{
		alreadyResolved: []memory.Memory{{ID: "M1", Content: content}},
		usefulnessErr:   errEvidenceUnavailable,
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	cls := &fakeClassifier{}

	res, reKept, err := Reassess(context.Background(), store, cls, "proj", true, Scope{}, logger)
	if err != nil {
		t.Fatalf("an unreadable audit must not fail the repair: %v", err)
	}
	if res.Cleared != 1 || len(reKept) != 1 {
		t.Fatalf("cleared=%d reKept=%v, want the repair to land with the audit unreadable", res.Cleared, reKept)
	}
	if got, want := store.kept["M1"], ContentHash(content); got != want {
		t.Errorf("stamped %q, want the bare content hash %q — a pass that could not read the evidence "+
			"must not stamp a verdict as judged under it", got, want)
	}
	if len(cls.askedFor) != 1 || cls.askedFor[0] != content {
		t.Errorf("asked about %q, want the note alone: an unreadable audit appends nothing", cls.askedFor)
	}
	if !strings.Contains(buf.String(), "usefulness evidence unavailable") {
		t.Errorf("expected the evidence read failure to be logged, log:\n%s", buf.String())
	}
}

// TestReassessClearErrorIsFatal: a failed repair must not be reported as a
// repair, so the error propagates instead of counting zero writes.
func TestReassessClearErrorIsFatal(t *testing.T) {
	content := "Cost estimate from May: $148/mo projected; actuals have since replaced it."
	store := &fakeStore{
		alreadyResolved: []memory.Memory{{ID: "m", Content: content}},
		clearErr:        errors.New("database is locked"),
	}
	cls := &fakeClassifier{drop: map[string]bool{}}

	if _, _, err := Reassess(context.Background(), store, cls, "proj", true, Scope{}, nil); err == nil {
		t.Fatal("Reassess: want the clear error propagated, got nil")
	}
	if len(store.markedKept) != 0 {
		t.Errorf("a failed clear must not cache KEEPs: %v", store.markedKept)
	}
}

// TestReassessMarkResolveKeptErrorWarns: once resolved_at is cleared the repair
// has landed; losing the derived cache entry only costs a re-ask next pass, so
// it warns rather than failing.
func TestReassessMarkResolveKeptErrorWarns(t *testing.T) {
	content := "Cost estimate from May: $148/mo projected; actuals have since replaced it."
	store := &fakeStore{
		alreadyResolved: []memory.Memory{{ID: "m", Content: content}},
		markErr:         errors.New("disk full"),
	}
	cls := &fakeClassifier{drop: map[string]bool{}}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	res, _, err := Reassess(context.Background(), store, cls, "proj", true, Scope{}, logger)
	if err != nil {
		t.Fatalf("Reassess must not fail on a cache-write error: %v", err)
	}
	if res.Cleared != 1 {
		t.Errorf("res.Cleared = %d, want 1", res.Cleared)
	}
	if !strings.Contains(buf.String(), "resolve kept cache write failed") {
		t.Errorf("expected a warning about the failed cache write, log:\n%s", buf.String())
	}
}

// TestReassessClassifierErrorIsFatal: a transport error leaves every verdict
// undecided, so nothing may be cleared — a partial repair would bury rows the
// harness never judged.
func TestReassessClassifierErrorIsFatal(t *testing.T) {
	content := "Cost estimate from May: $148/mo projected; actuals have since replaced it."
	store := &fakeStore{alreadyResolved: []memory.Memory{{ID: "m", Content: content}}}
	cls := &fakeClassifier{err: errors.New("boom")}

	if _, _, err := Reassess(context.Background(), store, cls, "proj", true, Scope{}, nil); err == nil {
		t.Fatal("Reassess: want the classifier error propagated, got nil")
	}
	if len(store.cleared) != 0 {
		t.Errorf("cleared = %v, want nothing on a fatal classify error", store.cleared)
	}
}

// TestReassessSkipsDeterministicallyAssertedRows: a row Run would immediately
// re-stamp for free — the older endpoint of a live 'supersedes'/'llm' link, or a
// row an unresolved correction still pairs with — is not a repair candidate.
// Clearing it would print "cleared resolved_at for 1" and have the next
// ordinary pass re-stamp it, so the operator is told a repair that did not
// happen. Such rows are reported as asserted instead (review finding on #643).
func TestReassessSkipsDeterministicallyAssertedRows(t *testing.T) {
	superseded := memory.Memory{ID: "superseded", Category: "gotcha", UpdatedAt: "2026-09-01 00:00:00",
		Content: "Cost estimate from May: $148/mo projected; actuals have since replaced it."}
	newer := memory.Memory{ID: "newer", Category: "gotcha", UpdatedAt: "2026-09-02 00:00:00",
		Content: "superseded the May cost estimate; the actuals document it now"}
	paired := memory.Memory{ID: "paired", Category: "gotcha", UpdatedAt: "2026-09-19 20:00:00",
		Content: "root cause: ledgerstate/imported_reward_inputs.go never sets CalculationVersion on imported reward_snapshot rows; unusable (closed)"}
	correction := memory.Memory{ID: "correction", Category: "gotcha", UpdatedAt: "2026-09-19 21:00:00",
		Content: "CORRECTION/RESOLUTION to the imported reward_snapshot P0: the bug IS ALREADY FIXED ON MAIN. Commit d646e680 adds CalculationVersion to ledgerstate/imported_reward_inputs.go. NO PR IS NEEDED FROM US."}
	vetoed := memory.Memory{ID: "vetoed", Category: "gotcha",
		Content: "NEVER run `dingo database restore` with source and target on the same spindle"}
	store := &fakeStore{
		alreadyResolved: []memory.Memory{superseded, paired, vetoed},
		// The correction is still live, so it is not in the resolved pool:
		// Run finds it through the unresolved pool, and so must the repair pass.
		candidates: []memory.Memory{newer, correction},
		links: []memory.Link{{
			SourceID: "newer", TargetID: "superseded", Relation: "supersedes", Source: "llm",
		}},
	}
	// The classifier would KEEP both asserted rows; the pass must not ask.
	cls := &fakeClassifier{drop: map[string]bool{}}

	res, reKept, err := Reassess(context.Background(), store, cls, "proj", true, Scope{}, nil)
	if err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	if res.Demoted != 2 {
		t.Errorf("res.Demoted = %d, want 2 (supersedes edge + correction pairing)", res.Demoted)
	}
	if len(reKept) != 1 || reKept[0].ID != "vetoed" {
		t.Fatalf("reKept = %v, want only the vetoed note", reKept)
	}
	if len(store.cleared) != 1 || store.cleared[0] != "vetoed" {
		t.Errorf("cleared = %v, want [vetoed]", store.cleared)
	}
	if cls.calls != 0 {
		t.Errorf("classifier calls = %d, want 0 (every row is settled for free)", cls.calls)
	}
}

// TestReassessCorrectionMustBeUnresolved: Run draws its corrections from the
// unresolved pool (correctionPairTargets(loaded, cands), where loaded is
// ResolveCandidates), so a correction that is itself resolved pairs nothing and
// the older row is repairable again. The floor must use the same pool, or it
// protects a row the next pass will not re-stamp and reports it under the wrong
// label (review finding on #643).
//
// The correction is judged RESOLVED here — it is the "already fixed on main, no
// PR needed" note resolve should have stamped anyway — so it stays resolved and
// asserts nothing. The companion case, where the correction is itself repaired
// and therefore leaves the pool, is TestReassessHoldsBackRowWhoseCorrectionIsRepaired.
func TestReassessCorrectionMustBeUnresolved(t *testing.T) {
	paired := memory.Memory{ID: "paired", Category: "gotcha", UpdatedAt: "2026-09-19 20:00:00",
		Content: "root cause: ledgerstate/imported_reward_inputs.go never sets CalculationVersion on imported reward_snapshot rows; unusable (closed)"}
	// The correction is itself resolved, so Run will never see it again.
	correction := memory.Memory{ID: "correction", Category: "gotcha", UpdatedAt: "2026-09-19 21:00:00",
		Content: "CORRECTION/RESOLUTION to the imported reward_snapshot P0: the bug IS ALREADY FIXED ON MAIN. Commit d646e680 adds CalculationVersion to ledgerstate/imported_reward_inputs.go. NO PR IS NEEDED FROM US."}
	store := &fakeStore{alreadyResolved: []memory.Memory{paired, correction}}
	cls := &fakeClassifier{drop: map[string]bool{correction.Content: true}}

	res, reKept, err := Reassess(context.Background(), store, cls, "proj", true, Scope{}, nil)
	if err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	if res.Demoted != 0 {
		t.Errorf("res.Demoted = %d, want 0 — a correction that stays resolved asserts nothing", res.Demoted)
	}
	if len(reKept) != 1 || reKept[0].ID != "paired" {
		t.Errorf("reKept = %v, want [paired] to be repairable", reKept)
	}
	if len(store.cleared) != 1 || store.cleared[0] != "paired" {
		t.Errorf("cleared = %v, want [paired]", store.cleared)
	}
}

// TestReassessHoldsBackRowWhoseCorrectionIsRepaired: a correction that is itself
// repaired in the same run resurrects the pairing it used to assert. Both rows
// leave the resolved pool, so the next ordinary pass finds the correction in
// ResolveCandidates and re-stamps the older row — the repair would be undone by
// the pass that follows it. So the older row is held back and reported as
// asserted, even though nothing asserts it yet (review finding on #643).
func TestReassessHoldsBackRowWhoseCorrectionIsRepaired(t *testing.T) {
	paired := memory.Memory{ID: "paired", Category: "gotcha", UpdatedAt: "2026-09-19 20:00:00",
		Content: "root cause: ledgerstate/imported_reward_inputs.go never sets CalculationVersion on imported reward_snapshot rows; unusable (closed)"}
	correction := memory.Memory{ID: "correction", Category: "gotcha", UpdatedAt: "2026-09-19 21:00:00",
		Content: "CORRECTION/RESOLUTION to the imported reward_snapshot P0: the bug IS ALREADY FIXED ON MAIN. Commit d646e680 adds CalculationVersion to ledgerstate/imported_reward_inputs.go. NO PR IS NEEDED FROM US."}
	other := memory.Memory{ID: "other", Category: "gotcha",
		Content: "Cost estimate from May: $148/mo projected; actuals have since replaced it."}
	store := &fakeStore{alreadyResolved: []memory.Memory{paired, correction, other}}
	// The classifier KEEPs all three: the correction is repairable, and so is
	// the cost estimate, which no correction touches.
	cls := &fakeClassifier{drop: map[string]bool{}}

	res, reKept, err := Reassess(context.Background(), store, cls, "proj", true, Scope{}, nil)
	if err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	if res.Demoted != 1 {
		t.Errorf("res.Demoted = %d, want 1 (the row a repaired correction would re-demote)", res.Demoted)
	}
	got := map[string]bool{}
	for _, m := range reKept {
		got[m.ID] = true
	}
	if got["paired"] {
		t.Error("paired must be held back: clearing the correction re-asserts the pairing")
	}
	if !got["correction"] || !got["other"] {
		t.Errorf("reKept = %v, want the correction and the cost estimate", reKept)
	}
	if len(store.cleared) != 2 || store.cleared[0] != "paired" || store.cleared[1] != "correction" {
		// Only the ids cleared matter; assert the set, not the order.
		cleared := map[string]bool{}
		for _, id := range store.cleared {
			cleared[id] = true
		}
		if len(cleared) != 2 || !cleared["correction"] || !cleared["other"] {
			t.Errorf("cleared = %v, want [correction other]", store.cleared)
		}
	}
	if _, ok := store.kept["paired"]; ok {
		t.Error("a held-back row must not get a KEEP cache entry it cannot act on")
	}
}

// TestReassessHoldsBackOnlyPrefilterPassingRows: Run's mechanism 2 iterates
// correctionPairTargets(loaded, cands) where cands is the keyword-prefiltered
// subset, so a row with no resolution keyword is never a pairing target however
// much a correction shares with it. Holding such a row back would be permanent
// and mislabelled — nothing asserts it, yet every later --reassess run would
// report it as asserted again, so no pass could ever repair it (review finding
// on #643).
func TestReassessHoldsBackOnlyPrefilterPassingRows(t *testing.T) {
	// No resolveKeywords entry, so the ordinary pass never considers it — the
	// same fixture TestRunCorrectionPairingSkipsLiveGotcha uses.
	live := memory.Memory{ID: "live", Category: "gotcha", UpdatedAt: "2026-09-01 00:00:00",
		Content: "re-bootstrap gotcha: ledgerstate/imported_reward_inputs.go mithril CalculationVersion never set on imported reward_snapshot rows — dingo-core-mithril-sync restarts"}
	correction := memory.Memory{ID: "correction", Category: "gotcha", UpdatedAt: "2026-09-19 00:00:00",
		Content: "CORRECTION/RESOLUTION to the imported reward_snapshot P0: the bug IS ALREADY FIXED ON MAIN. Commit d646e680 adds CalculationVersion to ledgerstate/imported_reward_inputs.go. NO PR IS NEEDED FROM US."}
	store := &fakeStore{alreadyResolved: []memory.Memory{live, correction}}
	// Both come back KEEP for free: the rule is vetoed ("never"), the
	// correction is not.
	cls := &fakeClassifier{}

	res, reKept, err := Reassess(context.Background(), store, cls, "proj", true, Scope{}, nil)
	if err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	if res.Demoted != 0 {
		t.Errorf("res.Demoted = %d, want 0 — a keyword-free row is never a pairing target", res.Demoted)
	}
	if len(reKept) != 2 {
		t.Errorf("reKept = %v, want both rows repairable", reKept)
	}
	if len(store.cleared) != 2 {
		t.Errorf("cleared = %v, want both rows", store.cleared)
	}
}

// TestReassessEmptyPool: a project with no resolved rows is a no-op that makes
// no harness call.
func TestReassessEmptyPool(t *testing.T) {
	store := &fakeStore{}
	cls := &fakeClassifier{}

	res, reKept, err := Reassess(context.Background(), store, cls, "proj", true, Scope{}, nil)
	if err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	if cls.calls != 0 || len(reKept) != 0 || res.Cleared != 0 {
		t.Errorf("calls=%d reKept=%v cleared=%d, want a no-op", cls.calls, reKept, res.Cleared)
	}
}
