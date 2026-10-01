package audit

import (
	"strings"
	"testing"
)

// A memory long enough that the three-token floor and the one-third fraction are both
// satisfiable by a sentence that genuinely discusses it, and distinctive enough
// that a sentence about something else cannot clear the bar by accident.
const negMemoryContent = "the go build cache lockfile directory lives under GOCACHE " +
	"and removing it forces a full recompile of every package"

// TestAnOrdinaryUseIsNotAContradiction: contradiction outranks every other
// verdict, so a loose negation bar reports ordinary use as "the agent found this
// memory wrong" — the one finding an operator acts on first. Each input here is
// text an agent writes CONSTANTLY, and each must land in used or ignored, never
// contradicted.
//
// The three cases are the three ways the old design over-fired: a tool argument
// carrying a negative-looking value, prose carrying a general negative, and prose
// naming the id while agreeing with it.
func TestAnOrdinaryUseIsNotAContradiction(t *testing.T) {
	cases := []struct {
		name  string
		apply func(*Signals)
		want  Outcome
	}{
		{
			// A tool argument, not a sentence. The fixture carries a REAL denial cue
			// and most of the memory's wording, because a weak fixture proves
			// nothing here: routing tool arguments through AddProse would add a
			// neg segment for this text, and the tightened bar would then report the
			// memory contradicted. Only a fixture that clears the bar can detect
			// that the routing was undone.
			name: "a tool argument carrying the memory's wording and a denial cue",
			apply: func(s *Signals) {
				s.AddToolArgs(`{"tool":"Edit","input":{"old_string":"` + negMemoryContent + `","note":"is obsolete"}}`)
			},
			want: OutcomeUsed,
		},
		{
			// A general negative in the agent's own prose: an instruction, not a
			// denial of the memory.
			name: "prose with a general negative",
			apply: func(s *Signals) {
				s.AddProse("I will not touch the " + negMemoryContent + " today.")
			},
			want: OutcomeUsed,
		},
		{
			// Naming the id while AGREEING. The old cue list held "actually", which
			// fires here.
			name: "prose naming the id and agreeing with it",
			apply: func(s *Signals) {
				s.AddProse("Per memory 4F3A9C1E7B2D8A6F5C0E1234AB5678EF, " +
					"actually that applies here.")
			},
			want: OutcomeIgnored,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestSignals(t)
			tc.apply(s)

			j := Judged{
				MemoryID: "4F3A9C1E7B2D8A6F5C0E1234AB5678EF",
				Content:  negMemoryContent,
			}
			if tc.want == OutcomeIgnored {
				// This case names the id but does not restate the memory, so the
				// identifier arm has nothing to match; assert only that it is not
				// contradicted, which is what the cue list was breaking.
				if s.contradicts(testTokens(j.Content), j.MemoryID) {
					t.Fatal("agreement that names the id was read as a contradiction")
				}
				return
			}
			v, ok := CompareAgainst(s, j)
			if !ok {
				t.Fatal("CompareAgainst refused to judge")
			}
			if v.Outcome == OutcomeContradicted {
				t.Fatalf("outcome = contradicted, want %s", tc.want)
			}
			if v.Outcome != tc.want {
				t.Errorf("outcome = %s, want %s", v.Outcome, tc.want)
			}
		})
	}
}

// TestAGenuineDenialIsStillAContradiction: the tightening must not cost the
// finding the audit exists for. The shape is the one a real correction takes —
// the memory's own distinctive wording, the agent asserting a different value
// for it, and an explicit denial.
func TestAGenuineDenialIsStillAContradiction(t *testing.T) {
	const id = "4F3A9C1E7B2D8A6F5C0E1234AB5678EF"

	cases := []struct {
		name  string
		prose string
	}{
		{
			name:  "is wrong, with a different value asserted",
			prose: "The " + negMemoryContent + " is wrong: GOCACHE now lives in /var/cache/go, not under $HOME.",
		},
		{
			name:  "no longer, with the memory's wording",
			prose: negMemoryContent + " no longer applies; the cache moved to /var/cache/go.",
		},
		{
			name:  "the id named in a sentence that denies something",
			prose: "Ignore memory " + id + ", that " + negMemoryContent + " is outdated.",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestSignals(t)
			s.AddProse(tc.prose)

			j := Judged{MemoryID: id, Content: negMemoryContent}
			v, ok := CompareAgainst(s, j)
			if !ok {
				t.Fatal("CompareAgainst refused to judge")
			}
			if v.Outcome != OutcomeContradicted {
				t.Fatalf("outcome = %s, want contradicted for %q", v.Outcome, tc.prose)
			}
		})
	}
}

// TestACueIsBoundToTheMemoryItDenies: #854. A cue denies SOMETHING, and the
// something has to be the memory it is filed against — which is a second
// requirement on top of the cue being a denial of a claim at all, and the one the
// old rule did not have.
//
// The id arm fired on any sentence that both named an id and carried a cue
// somewhere, so "Per <id>, I'll ignore the formatting" — an agent agreeing with
// the memory while talking about its own prose — was filed as contradicted, and
// because the cue list was matched as plain substrings, "ignore" also fired on
// "ignored" and "ignores" for the same reason. Contradiction outranks every other
// verdict, so both report ordinary use as the one finding an operator acts on
// first.
//
// The four cases are the rule from both sides, on both arms: a cue beside the id
// denies THAT memory, and a cue two words away denies something else.
func TestACueIsBoundToTheMemoryItDenies(t *testing.T) {
	const id = "4F3A9C1E7B2D8A6F5C0E1234AB5678EF"

	cases := []struct {
		name  string
		prose string
		want  Outcome
		sig   Signal
	}{
		{
			name:  "a cue about the agent's formatting, in a sentence naming the id",
			prose: "Per " + id + ", I'll ignore the formatting.",
			want:  OutcomeUsed,
			sig:   SignalIdentifier,
		},
		{
			name:  "a word that merely contains the cue, in a sentence naming the id",
			prose: "the ignored files stay ignored, per " + id,
			want:  OutcomeUsed,
			sig:   SignalIdentifier,
		},
		{
			name:  "the cue on the id it denies, before it",
			prose: "ignore " + id + ", it is outdated",
			want:  OutcomeContradicted,
			sig:   SignalNegation,
		},
		{
			name:  "the cue on the id it denies, after it",
			prose: id + " is false: the port is 8080",
			want:  OutcomeContradicted,
			sig:   SignalNegation,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestSignals(t)
			s.AddProse(tc.prose)

			j := Judged{MemoryID: id, Content: negMemoryContent}
			v, ok := CompareAgainst(s, j)
			if !ok {
				t.Fatal("CompareAgainst refused to judge")
			}
			if v.Outcome != tc.want {
				t.Fatalf("outcome = %s (%s) for %q, want %s", v.Outcome, v.Signal, tc.prose, tc.want)
			}
			if v.Signal != tc.sig {
				t.Errorf("signal = %q, want %q", v.Signal, tc.sig)
			}
		})
	}
}

// TestTheGapIsAWordGapAndNotAdjacency: cueGap is documented as how many WORDS may
// stand between a cue and the id it denies, and named for "ignore memory <id>" —
// a noun between the cue and the id. A bound test that reads the formula rather
// than the sentence lets the constant mean adjacency instead, and then the id arm
// silently loses that denial while the comment still claims it.
//
// The fixtures carry NO memory wording on purpose. With wording in them the
// fingerprint arm reaches the verdict too, so the test would pass on an id arm
// that binds nothing at all — which is exactly how the pre-existing fixture kept
// passing while this was broken.
func TestTheGapIsAWordGapAndNotAdjacency(t *testing.T) {
	const id = "4F3A9C1E7B2D8A6F5C0E1234AB5678EF"

	// A memory whose wording appears in none of these sentences, so the id arm is
	// the only one that can reach a verdict.
	unshared := "a memory whose wording the agent never repeated"

	cases := []struct {
		name  string
		prose string
		want  bool
	}{
		{
			name:  "a noun between the cue and the id",
			prose: "Ignore memory " + id + ".",
			want:  true,
		},
		{
			// The same gap on the other side of the cue: "is obsolete" is the cue
			// and one word stands between it and the id.
			name:  "a noun between the id and the cue",
			prose: id + " memory is obsolete",
			want:  true,
		},
		{
			// Two words stand between, which is one too many. A wider gap belongs to
			// #860 rather than here — the constant is one word and this pins that it
			// stays one word, so a later widening is a deliberate change rather than
			// an accident nobody noticed.
			name:  "two words between, beyond this rule's gap",
			prose: "disregard the memory " + id,
			want:  false,
		},
		{
			name:  "the cue beside the id",
			prose: "ignore " + id,
			want:  true,
		},
		{
			name:  "the cue after the id",
			prose: id + " is false",
			want:  true,
		},
		{
			name:  "a determiner between the id and the cue",
			prose: "memory " + id + " is wrong",
			want:  true,
		},
		{
			// The sentence the whole rule exists for. Two words stand between the
			// id and the cue, and the gap is one, so this must stay unbound however
			// the constant is spelled.
			name:  "two words between, an agreeing subject",
			prose: "Per " + id + ", I'll ignore the formatting.",
			want:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestSignals(t)
			s.AddProse(tc.prose)
			if got := s.contradicts(testTokens(unshared), id); got != tc.want {
				t.Errorf("contradicts = %v for %q, want %v", got, tc.prose, tc.want)
			}
		})
	}
}

// TestACueIsMatchedAsWholeWords: every cue is a run of WORDS, and the two that
// read as words — "ignore" and "is false" — are also the two a substring match
// finds inside ordinary ones ("ignored", "ignores", "falsehood", "falsely").
//
// That is a whole sentence's worth of false contradictions: an agent that names an
// id and then talks about its own ignored files has agreed with the memory, and
// the audit filed it as the agent having found the memory wrong.
//
// The check is per-cue in both directions. A list whose entries are matched as
// substrings passes "some cue works" for every one of them, and a fix that made
// the matcher stricter must not have made one of the real constructions
// unreachable, so each entry is asserted both as a run and inside a longer word.
func TestACueIsMatchedAsWholeWords(t *testing.T) {
	for _, cue := range negationCues {
		words := splitWords(cue)
		if strings.Join(words, " ") != cue {
			t.Errorf("cue %q is not a run of words (%q), so it cannot be matched as one", cue, words)
			continue
		}
		if !HasNegationCue("the " + cue + " of that thing") {
			t.Errorf("HasNegationCue(%q) = false: %q is a denial construction and must still match", cue, cue)
		}
		glued := "x" + cue + "y"
		if HasNegationCue("the " + glued + " of that thing") {
			t.Errorf("HasNegationCue matched %q inside %q, which denies nothing", cue, glued)
		}
	}
}

// TestTheCuesOwnWordsAreNotWhatACueIsBoundTo: a cue's own words are ordinary
// tokens — "ignore", "stale", "wrong", "superseded" all clear minTokenLen and none
// is a stopword — so holding them beside the memory's wording lets the memory's
// OWN CUE WORDS satisfy the binding. The agent then denies the changelog, quotes
// the memory, and the memory is filed contradicted for agreeing.
//
// The memory below contains "is false", which is why it is the right fixture: the
// sentence's cue and the memory's wording are the same two words, and nothing about
// the sentence binds the cue to the memory. The intervening words are what make the
// bar reachable at all — three-plus memory tokens and a third of them — so removing
// the binding has to be what stops this, and it has to stop it while the bar still
// holds.
func TestTheCuesOwnWordsAreNotWhatACueIsBoundTo(t *testing.T) {
	// One case per cue the reviewer named. Each memory's wording CONTAINS the cue
	// the sentence uses, and in each the sentence's cue is about something else, so
	// the only thing that could bind the two is the cue's own text.
	//
	// "notes aside" is what makes these the case they claim to be: it puts a word
	// the memory does not contain on the far side of the cue, so the sideward skip
	// settles there and NOTHING else in the sentence is bound to the cue. Without
	// it the skip reaches a memory word on its own, the cue really is bound to the
	// memory, and the fixture would pass on a binding that is too loose.
	//
	// The rest of each sentence quotes enough of the memory to clear the bar —
	// three-plus tokens and a third of them — so removing the binding has to be
	// what stops them, and it has to stop them while the bar still holds.
	cases := []struct {
		name  string
		mem   string
		prose string
	}{
		{
			name:  "the memory says is false, the sentence denies the summary with it",
			mem:   "the ghostctl stale lockfile directory is false weekly",
			prose: "the summary is false, notes aside ghostctl stale lockfile directory weekly entries",
		},
		{
			name:  "the memory says is obsolete, the sentence denies the summary with it",
			mem:   "the ghostctl lockfile directory is obsolete weekly",
			prose: "the summary is obsolete, notes aside ghostctl lockfile directory weekly entries",
		},
		{
			name:  "the memory says wrong, the sentence denies it with wrong",
			mem:   "the changelog entry that says wrong weekly",
			prose: "that is wrong, notes aside the changelog entry weekly lines",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			toks := testTokens(tc.mem)
			s := newTestSignals(t)
			s.AddProse(tc.prose)
			if !s.matches(toks) {
				t.Fatal("the fixture does not clear the token arm's bar, so it proves nothing about the binding")
			}
			if s.contradicts(toks, "") {
				t.Error("the memory's own cue word satisfied the binding; the cue was bound to the summary")
			}
		})
	}

	// The other edge, on one of them: the same words with a memory word actually
	// beside the cue ARE a denial, so the fix cannot be "drop the condition".
	const mem = "the ghostctl stale lockfile directory is false weekly, vendored packaging needs it"
	toks := testTokens(mem)
	near := newTestSignals(t)
	near.AddProse("the ghostctl stale lockfile directory is false weekly")
	if !near.contradicts(toks, "") {
		t.Error("a memory's own wording beside the cue is not a denial")
	}
}

// TestASidewardSkipStepsOverASecondCue: a sentence can carry two cues, and the
// skip that finds what the first one is about lands on the second one's own text
// ("the changelog is wrong, ignore the vendored docs" — "is wrong" is about the
// changelog, and the first distinctive word after it is "ignore"). "ignore" is a
// token like any other, so recording it binds the first cue to a memory whose
// wording happens to say "ignore", which is the same false contradiction as
// TestTheCuesOwnWordsAreNotWhatACueIsBoundTo reached by a different route.
//
// The memory says "ignore" and the sentence says "ignore", and neither is about the
// other — which is why this is checked separately from the cue's own words: the
// skip has to step over a cue it finds, not merely start after one.
func TestASidewardSkipStepsOverASecondCue(t *testing.T) {
	// The memory's first token is "ignore", and the sentence's SECOND cue is
	// "ignore" — so a skip that stops there binds the first cue to a word it is not
	// about, and nothing else in the sentence binds it at all ("changelog" and
	// "vendored" are in no memory).
	const mem = "ignore the stale ghostctl cache directory weekly"
	toks := testTokens(mem)

	s := newTestSignals(t)
	s.AddProse("the changelog is wrong, ignore the vendored docs, " +
		"and the stale ghostctl cache directory weekly is fine")
	if !s.matches(toks) {
		t.Fatal("the fixture does not clear the token arm's bar, so it proves nothing about the skip")
	}
	if s.contradicts(toks, "") {
		t.Error("a sideward skip landed on a second cue's own word and bound the first cue to the memory")
	}
}

// TestTheFingerprintArmNeedsTheCueBesideTheMemorysOwnWords: the binding is this
// arm's rule too, and in the same direction. A cued sentence can clear the token
// bar and still be denying something else: the fixture below denies a formatter
// and then quotes the memory verbatim, and the memory's own wording is not what
// the cue is bound to — a claim about this memory the agent never made.
//
// The second half is the other edge of the same rule, and the reason the binding
// is a named rule rather than "the same sentence": the identical words with the
// cue beside them ARE a denial.
//
// The first fixture's cue is bound to "formatter", which is why it reads the way
// it does. A cue followed by a colon or a dash and then the restatement ("is
// wrong: <the memory's wording>") IS a denial — splitWords drops the punctuation,
// so a clause boundary is not a word to be counted, and those are pinned by the
// pre-existing tests that quote the cue at the head of the sentence.
func TestTheFingerprintArmNeedsTheCueBesideTheMemorysOwnWords(t *testing.T) {
	toks := memTokens(t)

	far := newTestSignals(t)
	far.AddProse("ignore the formatter entirely, " + memContent)
	if !far.matches(toks) {
		t.Fatal("the fixture does not clear the token arm's bar, so it proves nothing about the binding")
	}
	if far.contradicts(toks, "") {
		t.Error("a cue bound to another subject contradicted a memory the sentence only quoted")
	}

	near := newTestSignals(t)
	near.AddProse("the opencode plugin materializes its transcript under mkdtemp is wrong")
	if !near.contradicts(toks, "") {
		t.Error("the same words with the cue beside them are not a denial")
	}
}

// TestTheBindingSurvivesTheSidecar: the hook and the child are separate
// processes, so what the comparison asks about a cue has to be on the wire like
// everything else it needs. Both halves of it are new — the ids a cue is bound
// to, and the fingerprints of the words it is bound to — and the second is a
// third field kind on the `neg` line, which is why the line is marked rather than
// read by shape alone.
//
// The third case is the one a round trip could plausibly get wrong in the
// permissive direction: a sentence naming an id its cue is NOT bound to must come
// back without that id attached to a denial.
func TestTheBindingSurvivesTheSidecar(t *testing.T) {
	const (
		denied = "4F3A9C1E7B2D8A6F5C0E1234AB5678EF"
		quoted = "5A2B4C6D8E0F1A3B5C7D9E0F1A2B3C4D"
		// Words no sentence below shares, so the id arm is the only one that can
		// reach a verdict in these two assertions.
		unshared = "a memory whose wording the agent never repeated"
	)

	s := newTestSignals(t)
	s.AddProse("ignore " + denied + ", it is outdated")
	s.AddProse("the opencode plugin materializes its transcript under mkdtemp is wrong")
	s.AddProse("Per " + quoted + ", I'll ignore the formatting.")

	path, err := WriteSidecar(t.TempDir(), s)
	if err != nil {
		t.Fatalf("WriteSidecar: %v", err)
	}
	got, err := ReadSidecar(path, testHasher)
	if err != nil {
		t.Fatalf("ReadSidecar: %v", err)
	}

	if !got.contradicts(testTokens(unshared), denied) {
		t.Error("the id the cue was bound to did not survive the sidecar")
	}
	if !got.contradicts(memTokens(t), "") {
		t.Error("the words the cue was bound to did not survive the sidecar")
	}
	if got.contradicts(testTokens(unshared), quoted) {
		t.Error("an id the cue was NOT bound to came back as a denial")
	}
}

// TestTheSidecarVersionIsBumpedRatherThanReused: the binding is a THIRD field kind
// on the `neg` line, so a file written before it is missing the field the
// comparison now asks about — and it would parse all the same, because the two
// shapes it already had did not move. Read back, it carries fingerprints and
// nothing bound to any cue, so every contradiction in that turn's file goes
// unfound and the audit reports clean.
//
// That is why the header is bumped rather than reused: an unreadable format says
// so out loud and costs one turn's audit, which is what a sidecar this build
// cannot read is already worth. The two headers must differ and the refusal must
// name BOTH, because the path arrived on a command line and a reader holding only
// the new format would otherwise have nothing to go on.
func TestTheSidecarVersionIsBumpedRatherThanReused(t *testing.T) {
	if SidecarHeader == sidecarV1 {
		t.Fatalf("SidecarHeader and sidecarV1 are both %q, so a file written before the binding was introduced parses as one this build wrote", SidecarHeader)
	}

	path := t.TempDir() + "/ghost-audit-old.signals"
	if err := writeFileString(path, sidecarV1+"\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := ReadSidecar(path, testHasher)
	if err == nil {
		t.Fatal("ReadSidecar accepted a file written before the cue binding existed; every contradiction in it would be silently unfound")
	}
	for _, want := range []string{SidecarHeader, sidecarV1} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ReadSidecar error %q does not name %q, so a reader cannot tell which format it was refused against", err, want)
		}
	}
}

// TestTheNegationCuesAreAllDenialsOfAClaim: each cue is a construction where a
// denial of some specific claim is grammatically required, so it cannot fire on
// an instruction or an agreement. This is the property the reviewer found broken
// — "not ", "n't ", "actually" and friends were cues, and each of them fires on
// ordinary text.
//
// The check is per-cue rather than on the whole list, because the defect was
// several entries in it, and a test that only asserted "some cue works" would
// have passed with all four of them still present.
func TestTheNegationCuesAreAllDenialsOfAClaim(t *testing.T) {
	// Text that contains each rejected cue in a NON-denial sense. If any of these
	// is reported as negating, a weak cue is still in the list.
	benign := []string{
		"not", "n't", "never", "actually", "correction", "stale", "false",
		"instead of", "rather than", "wrong", "incorrect", "obsolete",
		"outdated", "deprecated", "superseded",
	}
	for _, word := range benign {
		// Build a sentence that uses the word the way ordinary prose does.
		sentence := "I will " + word + " remove the cache lockfile directory before rebuilding"
		if HasNegationCue(sentence) {
			t.Errorf("HasNegationCue(%q) = true, but this is not a denial of any claim", sentence)
		}
	}
}

// TestANegationNeedsTheMemorysOwnWording: the cue alone is not enough — the
// sentence must be ABOUT this memory.
//
// This is the case a >=2 bar could not tell from a real denial, and it is why the
// negation arm uses the token arm's own thresholds. The memory has exactly four
// distinctive tokens, and the denial below names TWO of them: it passes a
// `matched >= 2` bar and fails the `matched >= 3` one. The two words are shared
// because the denial is ABOUT THE SAME FILESYSTEM AREA, not because it denies
// this claim — and an audit that files "the agent corrected this memory" for a
// sentence about a different setting is the false contradiction the whole
// tightening exists to stop.
func TestANegationNeedsTheMemorysOwnWording(t *testing.T) {
	const id = "4F3A9C1E7B2D8A6F5C0E1234AB5678EF"
	j := Judged{MemoryID: id, Content: "the mkdtemp directory is removed on hook close"}

	s := newTestSignals(t)
	// "is wrong" — a real denial cue — naming mkdtemp and directory, but denying
	// a DIFFERENT claim about them.
	s.AddProse("The mkdtemp directory path in the sweep is wrong: /var/tmp is not writable here.")

	if s.contradicts(testTokens(j.Content), j.MemoryID) {
		t.Error("a denial sharing two words of a four-token memory was read as a contradiction of it")
	}
}

// TestANegationNeedsTheMemorysOwnWordingStronglyHeld: the wider case, where the
// two topics share nothing at all. Kept as its own assertion because the two
// neighbouring fixtures differ in what they must not be confused with — this one
// is about a denial of an unrelated subject, and the one above is about a denial
// that happens to share two words.
func TestANegationNeedsTheMemorysOwnWordingStronglyHeld(t *testing.T) {
	const id = "4F3A9C1E7B2D8A6F5C0E1234AB5678EF"
	j := Judged{MemoryID: id, Content: negMemoryContent}

	s := newTestSignals(t)
	s.AddProse("The linter configuration is wrong: it should use golangci-lint 2.x.")

	if s.contradicts(testTokens(j.Content), j.MemoryID) {
		t.Error("a denial about an unrelated subject contradicted this memory")
	}

	// And the agent did use the memory, so the verdict is used.
	s.AddProse("Checking the " + negMemoryContent + " now.")
	v, ok := CompareAgainst(s, j)
	if !ok {
		t.Fatal("CompareAgainst refused to judge")
	}
	if v.Outcome != OutcomeUsed {
		t.Errorf("outcome = %s, want used", v.Outcome)
	}
}

// TestTheSidecarStillRecordsNegationSegmentsWithoutToolArguments: the fix routes
// tool arguments away from the negation arm, so a sidecar round-trip must still
// carry a prose denial — otherwise the tightening would have quietly disabled the
// arm entirely.
func TestTheSidecarStillRecordsNegationSegmentsWithoutToolArguments(t *testing.T) {
	dir := t.TempDir()

	s := newTestSignals(t)
	s.AddProse("The " + negMemoryContent + " is wrong: GOCACHE moved to /var/cache/go.")
	// A tool argument carrying BOTH a real denial cue and the memory's own
	// distinctive words. If this reached the negation arm it would be the second
	// segment, and its presence is what the assertion below detects.
	s.AddToolArgs(`{"tool":"Edit","input":{"old_string":"` + negMemoryContent + `","note":"is obsolete"}}`)

	path, err := WriteSidecar(dir, s)
	if err != nil {
		t.Fatalf("WriteSidecar: %v", err)
	}
	got, err := ReadSidecar(path, testHasher)
	if err != nil {
		t.Fatalf("ReadSidecar: %v", err)
	}
	if len(got.negated) != 1 {
		t.Fatalf("negated segments after round trip = %d, want 1 (prose only)", len(got.negated))
	}
	// The one segment must be the PROSE denial, so it must clear the bar against
	// the memory and the tool argument's copy must not have doubled the list.
	j := Judged{MemoryID: "4F3A9C1E7B2D8A6F5C0E1234AB5678EF", Content: negMemoryContent}
	if !got.contradicts(testTokens(j.Content), j.MemoryID) {
		t.Error("the prose denial did not survive the sidecar")
	}
}
