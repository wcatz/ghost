package audit

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// memContent is the memory the tests below audit. Its wording is deliberately
// specific — the audit matches on distinctive tokens, so a memory whose words
// are all common would be judged by a rule no test could pin.
const memContent = "The opencode plugin materializes its transcript under mkdtemp and rm-rfs the directory on hook close"

// memWords is DistinctTokens' output for memContent, written out so the fixture
// and the rule cannot drift apart silently: a change to either fails here rather
// than quietly changing what every other test below is measuring. "under" is
// absent because it is a stopword, which is the half of the rule that is easy to
// get wrong in the direction of counting everything.
var memWords = []string{"opencode", "plugin", "materializes", "transcript", "mkdtemp", "directory", "hook", "close"}

// testMemoryID is an id-shaped token, which the arms match by shape rather than
// by overlap with anything.
const testMemoryID = "D20E133860CC4AFE38B485AD5371BA59"

// memTokens is the same memory's fingerprints, which every judgement below is
// expressed against.
func memTokens(t *testing.T) []string {
	t.Helper()
	got := testTokens(memContent)
	want := make([]string, len(memWords))
	for i, w := range memWords {
		want[i] = testHasher.Fingerprint(w)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("testTokens(%q) = %d fingerprints, want %d in first-seen order %v", memContent, len(got), len(want), memWords)
	}
	return got
}

func TestDistinctTokensKeepsLongWordsAndDropsStopwords(t *testing.T) {
	got := testTokens("The opencode plugin materializes 123 the AND of a with about mkdtemp")
	want := []string{"opencode", "plugin", "materializes", "mkdtemp"}
	if len(got) != len(want) {
		t.Fatalf("DistinctTokens = %d fingerprints, want %d (%v)", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != testHasher.Fingerprint(w) {
			t.Errorf("token %d = %s, want the fingerprint of %q", i, got[i], w)
		}
	}
}

func TestDistinctTokensDeduplicatesInFirstSeenOrder(t *testing.T) {
	got := testTokens("mkdtemp rm-rfs mkdtemp hook mkdtemp close")
	want := []string{testHasher.Fingerprint("mkdtemp"), testHasher.Fingerprint("hook"), testHasher.Fingerprint("close")}
	if len(got) != len(want) {
		t.Fatalf("DistinctTokens = %v, want %d distinct fingerprints", got, len(want))
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("token %d = %s, want the fingerprint of %q (first-seen order)", i, got[i], w)
		}
	}
}

// TestDistinctTokensIsCaseInsensitive: a memory and the agent who restates it
// do not agree on capitalisation, and a rule keyed on exact case would report
// every mixed-case restatement as ignored.
func TestDistinctTokensIsCaseInsensitive(t *testing.T) {
	if testHasher.Fingerprint("Mkdtemp") != testHasher.Fingerprint("mkdtemp") {
		t.Error("Fingerprint is case-sensitive; a restated sentence that capitalises a word would never match")
	}
}

// TestAddProseKeepsIDsOutOfTheTokenSet: an id is matched by shape, not by
// overlap, so hashing one as a word would let a 32-character hex string stand in
// for the memory's own wording.
func TestAddProseKeepsIDsOutOfTheTokenSet(t *testing.T) {
	s := newTestSignals(t)
	s.AddProse("looked at " + testMemoryID + " and moved on")
	if !s.HasID(testMemoryID) {
		t.Error("HasID = false for an id the agent named")
	}
	if s.matches(testTokens(testMemoryID)) {
		t.Error("an id read as a word; the token arm must not be satisfied by the id itself")
	}
}

// TestHasIDIsCaseInsensitive covers the same reasoning as
// TestDistinctTokensIsCaseInsensitive for the identifier arm: Ghost mints ids
// in upper case and a host may render them in either.
func TestHasIDIsCaseInsensitive(t *testing.T) {
	s := newTestSignals(t)
	s.AddProse("see " + strings.ToLower(testMemoryID))
	if !s.HasID(testMemoryID) {
		t.Error("HasID = false for an id the transcript spells in lower case")
	}
}

// TestNegationCue holds the cue list to the property it has to have: every entry
// is a construction where denying some claim is grammatically required.
//
// The second half is the load-bearing one, and it is not decoration. The list
// originally held "not ", "n't ", "never ", "instead of", "correction",
// "actually" and "wrong" as bare cues, and each of those fires on ordinary text —
// an instruction ("we don't rm -rf the directory"), a mention ("it is not
// unrelated"), or outright AGREEMENT ("actually the path survives"). Since
// contradiction outranks every other verdict, each of those is an ordinary use
// reported as a finding the operator would act on. A cue list is only defensible
// while every entry in it is a denial of a claim.
func TestNegationCue(t *testing.T) {
	cued := []string{
		"that is no longer true",
		"that reading is wrong",
		"the key is obsolete",
		"the transcript path is stale",
		"that flag is incorrect",
		"the cache directory setting is false",
		"that value is not true",
		"the override is no longer correct",
		"the limit no longer applies",
		"the env override is deprecated",
		"the pointer table is superseded",
		"ignore that memory",
		"disregard the earlier note",
	}
	for _, seg := range cued {
		if !HasNegationCue(seg) {
			t.Errorf("HasNegationCue(%q) = false, want true", seg)
		}
	}
	clear := []string{
		"the opencode plugin materializes its transcript under mkdtemp",
		"i checked the directory and it is gone",
		"nothing here contradicts the memory",
		// General negatives: an instruction or a mention, never a denial.
		"the transcript is not materialised anywhere else",
		"we don't rm-rf the directory",
		"it never lands on disk",
		"use the state directory instead of mkdtemp",
		"i will not touch the cache lockfile directory today",
		// Agreement, which the old "actually" cue read as denial.
		"correction: the plugin owns the sweep",
		"actually the path survives",
		// A bare adjective with no copula: "wrong" on its own is a value, not a
		// denial, and the list keeps only the copula forms.
		"the wrong directory was passed to rm-rf",
	}
	for _, seg := range clear {
		if HasNegationCue(seg) {
			t.Errorf("HasNegationCue(%q) = true, want false", seg)
		}
	}
}

// TestAddProseAttributesANegationToItsOwnSentence is the property the whole
// contradicted verdict rests on: the cue and the memory have to be in the SAME
// sentence, or every memory mentioned anywhere in a document that contains one
// "actually" would be reported as contradicted.
// The cued sentence below deliberately shares NO distinctive token with the
// memory, and the sentence above it carries the memory's whole wording. That is
// what makes this a test of ATTRIBUTION rather than of the token bar: an
// implementation that pooled every cued sentence's fingerprints with the whole
// transcript's would find the cue and the memory's tokens together and answer
// true, which is the failure the sentence split exists to prevent.
func TestAddProseAttributesANegationToItsOwnSentence(t *testing.T) {
	toks := memTokens(t)
	s := newTestSignals(t)
	s.AddProse("First, " + memContent + ". " +
		"Second, that recollection is wrong; the cleanup helper owns this now. " +
		"Third, mkdtemp is a POSIX call.")
	if s.contradicts(toks, "") {
		t.Error("contradicts = true for a cue in a different sentence than the memory's wording")
	}
	if !s.matches(toks) {
		t.Error("matches = false: the same sentence's tokens must not be discarded, only attributed as a negation")
	}
}

func TestAddSaveArgsIsNotUsage(t *testing.T) {
	toks := memTokens(t)
	s := newTestSignals(t)
	s.AddSaveArgs("what I learned: " + memContent)
	if s.matches(toks) {
		t.Error("a save's own content counted as usage; the superseded-in-session bucket could then never be non-empty")
	}
	if !s.matchesSaves(toks) {
		t.Error("a save restating the memory is not recognised, so nothing is ever superseded in-session")
	}
}

// TestAddToolArgsIsUsage: a tool call that is not a save carries the agent's own
// words like any other prose, so an Edit whose old_string IS the memory's wording
// is a use. The fixture quotes the whole memory rather than one word of it
// because the token arm asks for a third of the memory and never fewer than
// three — an Edit naming a single distinctive word is genuinely not evidence, and
// asserting otherwise here would pin a threshold the tests elsewhere hold.
func TestAddToolArgsIsUsage(t *testing.T) {
	toks := memTokens(t)
	s := newTestSignals(t)
	s.AddToolArgs(`{"tool":"Edit","input":{"old_string":"` + memContent + `"}}`)
	if !s.matches(toks) {
		t.Error("a non-save tool call's arguments are the agent's own words and must count as usage")
	}
	if s.matchesSaves(toks) {
		t.Error("a non-save tool call must not be read as a save")
	}
}

func TestSignalsDegraded(t *testing.T) {
	s := newTestSignals(t)
	if reason, ok := s.Degraded(); ok || reason != "" {
		t.Errorf("Degraded() = %q,%v on a clean scan, want \"\",false", reason, ok)
	}
	s.MarkDegraded("transcript line exceeds the memory ceiling")
	if reason, ok := s.Degraded(); !ok || reason != "transcript line exceeds the memory ceiling" {
		t.Errorf("Degraded() = %q,%v, want the marked reason", reason, ok)
	}
}

// TestSignalsRoundTripThroughTheSidecar: the hook writes these and a DETACHED
// child reads them, because an adapter-materialized transcript is swept as soon
// as the hook returns. Anything the comparison needs must survive that trip, and
// the trip must not become a channel for transcript text.
func TestSignalsRoundTripThroughTheSidecar(t *testing.T) {
	s := newTestSignals(t)
	s.AddProse("the opencode plugin materializes its transcript under mkdtemp and mentions " + testMemoryID)
	// Enough of the memory's own words to clear the negation arm's bar rather than
	// merely carrying a cue — the point of the case is that the cued segment
	// survives the round trip, and a segment under the bar would not. The bar is
	// the SAME one the used arm applies (>=3 fingerprints and >=a third of the memory),
	// because a denial needs to be about the memory for the same reason a use does.
	s.AddProse("that is wrong, the opencode plugin does not materialize its transcript under mkdtemp")
	s.AddSaveArgs("the opencode plugin materializes its transcript under mkdtemp")
	s.MarkDegraded("scan transcript: truncated")

	path, err := WriteSidecar(t.TempDir(), s)
	if err != nil {
		t.Fatalf("WriteSidecar: %v", err)
	}
	got, err := ReadSidecar(path, testHasher)
	if err != nil {
		t.Fatalf("ReadSidecar: %v", err)
	}
	toks := memTokens(t)
	if !got.HasID(testMemoryID) {
		t.Error("the id did not survive the sidecar")
	}
	if !got.matches(toks) {
		t.Error("the prose tokens did not survive the sidecar")
	}
	if !got.contradicts(toks, "") {
		t.Error("the negated segment did not survive the sidecar")
	}
	if !got.matchesSaves(toks) {
		t.Error("the save tokens did not survive the sidecar")
	}
	if reason, ok := got.Degraded(); !ok || reason != "scan transcript: truncated" {
		t.Errorf("Degraded() = %q,%v, want the marked reason", reason, ok)
	}
}

// TestSidecarCarriesNoTranscriptText: the sidecar is a file, on disk, for the
// length of a session — it must hold fingerprints and ids and nothing that
// could be read back as what the agent said.
func TestSidecarCarriesNoTranscriptText(t *testing.T) {
	s := newTestSignals(t)
	s.AddProse("mangoes are the only durable fruit in this transcript")
	s.AddSaveArgs("bananas belong in the same sentence as a save")
	path, err := WriteSidecar(t.TempDir(), s)
	if err != nil {
		t.Fatalf("WriteSidecar: %v", err)
	}
	raw, err := readFileString(path)
	if err != nil {
		t.Fatalf("read the sidecar: %v", err)
	}
	for _, word := range []string{"mangoes", "durable", "bananas"} {
		if strings.Contains(raw, word) {
			t.Errorf("the sidecar carries the transcript word %q", word)
		}
	}
}

// TestSignalsEmptyIsWhatTheHookRefusesToCompare: an empty sidecar is the one
// reading that would make the report confidently wrong. Compared against, it
// files every kept memory as "ignored" — a claim that the agent used nothing,
// produced by a scan that read nothing (a transcript whose assistant authored
// nothing, or one this build has no audit scanner for). So the hook asks this
// before writing the file, and the degradation reason does not make an empty
// scan worth comparing: a partial read of nothing is still nothing.
func TestSignalsEmptyIsWhatTheHookRefusesToCompare(t *testing.T) {
	if !(newTestSignals(t)).Empty() {
		t.Error("a fresh Signals must be empty")
	}
	onlyDegraded := newTestSignals(t)
	onlyDegraded.MarkDegraded("scan transcript: stopped before the end (unexpected EOF)")
	if !onlyDegraded.Empty() {
		t.Error("a scan that found nothing is empty even when it says it stopped early: " +
			"the reason is a caveat on a comparison, and there is nothing to compare")
	}
	for _, tc := range []struct {
		name  string
		add   func(*Signals)
		empty bool
	}{
		{"prose", func(s *Signals) { s.AddProse("the migration runs before the backup") }, false},
		{"an id alone", func(s *Signals) { s.AddID(testMemoryID) }, false},
		{"a save's arguments alone", func(s *Signals) { s.AddSaveArgs("pinned versions come from the lockfile") }, false},
		// Prose made entirely of stopwords fingerprints to nothing, so it is the
		// same empty scan: the hook must not write a sidecar for it either.
		{"prose that is all stopwords", func(s *Signals) { s.AddProse("the of and it is as that with") }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestSignals(t)
			tc.add(s)
			if got := s.Empty(); got != tc.empty {
				t.Errorf("Empty() = %v, want %v", got, tc.empty)
			}
		})
	}
}

// TestSweepStaleAppliesThisPackagesOwnAge: the lifecycle reaps with the age the
// sidecar contract names, and a caller that picked its own number could sweep a
// sidecar a slow child had not read yet. So the exported entry point carries the
// policy with it and the raw one stays available to a test that needs to age a
// file by an exact amount.
func TestSweepStaleAppliesThisPackagesOwnAge(t *testing.T) {
	dir := t.TempDir()
	stale := dir + "/ghost-audit-stale.signals"
	if err := writeFileString(stale, SidecarHeader+"\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := touchOlderThan(stale, sidecarStaleAfter+time.Hour); err != nil {
		t.Fatalf("age the sidecar: %v", err)
	}
	n, err := SweepStale(dir)
	if err != nil {
		t.Fatalf("SweepStale: %v", err)
	}
	if n != 1 {
		t.Errorf("SweepStale removed %d file(s), want 1 — the age is the package's, not the caller's", n)
	}
}

func TestReadSidecarRejectsAnUnknownHeader(t *testing.T) {
	path := t.TempDir() + "/ghost-audit-bogus.signals"
	if err := writeFileString(path, "not a sidecar\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := ReadSidecar(path, testHasher); err == nil {
		t.Error("ReadSidecar accepted a file it did not write; a stale or foreign path would silently audit nothing")
	}
}

// TestSweepSidecarsRemovesOnlyStaleFiles: the child deletes its own sidecar, but
// a child that never started (a refused spawn, a crash between the two) leaves
// one behind, and the next invocation is what has to clean it up.
func TestSweepSidecarsRemovesOnlyStaleFiles(t *testing.T) {
	dir := t.TempDir()
	fresh := dir + "/ghost-audit-fresh.signals"
	stale := dir + "/ghost-audit-stale.signals"
	unrelated := dir + "/keep-me"
	for _, p := range []string{fresh, stale, unrelated} {
		if err := writeFileString(p, SidecarHeader+"\n"); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	if err := touchOlderThan(stale, 2*sidecarStaleAfter); err != nil {
		t.Fatalf("age the stale sidecar: %v", err)
	}
	n, err := SweepSidecars(dir, sidecarStaleAfter)
	if err != nil {
		t.Fatalf("SweepSidecars: %v", err)
	}
	if n != 1 {
		t.Errorf("SweepSidecars removed %d file(s), want 1", n)
	}
	if _, err := readFileString(fresh); err != nil {
		t.Error("the sweep removed a sidecar a running child is about to read")
	}
	if _, err := readFileString(unrelated); err != nil {
		t.Error("the sweep removed a file that is not a sidecar")
	}
	if _, err := readFileString(stale); err == nil {
		t.Error("the stale sidecar survived the sweep")
	}
}
