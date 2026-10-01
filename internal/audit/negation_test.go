package audit

import (
	"slices"
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
			// Two words stand between, which is one too many for ARBITRARY words —
			// and these two are arbitrary, which is what this case is for. A wider gap
			// that does admit determiners and nouns is #860 and is held by
			// TestTheGapWidensOnlyAcrossAClosedSetOfWords, so that the constant here
			// still pins what it pinned: one word, for anything the closed set does not
			// name.
			name:  "two words between, neither of them a closed-set word",
			prose: "disregard the formatter config " + id,
			want:  false,
		},
		{
			// And the same sentence from the other side, where a THREE-word gap of
			// arbitrary words still must not bind: the widening is worth a named set
			// of words, not a longer distance.
			name:  "three arbitrary words between, beyond this rule's gap",
			prose: "ignore the formatter config entirely " + id,
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

// TestTheGapWidensOnlyAcrossAClosedSetOfWords: #860, following #858. cueGap is one
// word, which is the right side of the trade to be on — a denial it misses is a
// contradiction not filed, the honest direction — but one word is narrower than
// English, because a denial routinely puts a determiner and a noun between the cue
// and the memory it denies:
//
//	"Memory <id> is now obsolete"
//	"disregard the memory <id>"
//	"ignore the advice in <id>"
//
// Each of those is a genuine denial that came back `used`.
//
// So the gap widens, and what it widens ACROSS is the whole of the fix: a small
// CLOSED set of words, or none at all. Widening cueGap for arbitrary words would
// trade the miss for the false contradiction #858 exists to stop — "Per <id>, I'll
// ignore the formatting" puts two ordinary words between an id and a cue, and they
// are not in the set, so it stays unbound however wide the distance is.
//
// The fixtures carry NO memory wording, so the id arm is the only one that can
// reach a verdict, and the negatives use the same memory as the #858 cases they
// came from: a fixture that could pass on the fingerprint arm proves nothing about
// a gap.
func TestTheGapWidensOnlyAcrossAClosedSetOfWords(t *testing.T) {
	const id = "4F3A9C1E7B2D8A6F5C0E1234AB5678EF"

	// A memory whose wording appears in none of the sentences below.
	unshared := "a memory whose wording the agent never repeated"

	t.Run("a denial the closed set reaches", func(t *testing.T) {
		cases := []struct {
			name  string
			prose string
		}{
			{
				// Two words between the cue and the id, both of them in the set.
				name:  "a determiner and a noun between the cue and the id",
				prose: "disregard the memory " + id,
			},
			{
				// Three, and this is the sentence that fixes the width of the
				// allowance: a determiner, the noun an agent actually reaches for
				// when it means the memory, and a preposition. Two closed-set words
				// is one too few for it, so the constant is not a rounder number.
				name:  "a determiner, a noun and a preposition between them",
				prose: "ignore the advice in " + id,
			},
			{
				// The other direction, and the shape whose miss is on the CUE side:
				// the cue is "is obsolete" with an adverb inside it, so it is not a
				// run of consecutive words at all until the closed set is transparent
				// inside a cue as well as around one. The id is adjacent here, so
				// this isolates the cue run from the gap.
				name:  "the cue split by a closed-set word, then the id",
				prose: "Memory " + id + " is now obsolete",
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				s := newTestSignals(t)
				s.AddProse(tc.prose)
				if !s.contradicts(testTokens(unshared), id) {
					t.Errorf("contradicts = false for %q: a denial with only closed-set words between the cue and the id is not bound", tc.prose)
				}
			})
		}
	})

	t.Run("a cue whose own words are not adjacent", func(t *testing.T) {
		// "Memory <id> is now obsolete" misses on the CUE side of the same problem:
		// the cue is "is obsolete", and an adverb stands inside it. So the closed
		// set is transparent inside a cue run as well as around it, and the run
		// still cannot be a word apart from any shape.
		if !HasNegationCue("the memory is now obsolete") {
			t.Error(`HasNegationCue("the memory is now obsolete") = false: a closed-set word inside a cue must not stop the cue matching`)
		}
		if HasNegationCue("the memory is very obsolete") {
			t.Error(`HasNegationCue("the memory is very obsolete") = true, but "very" is not a closed-set word and this is not a denial construction`)
		}
		// And the bound itself, from the side past it: TWO closed-set words inside
		// one cue run is more than the rule allows, so the allowance is one word
		// rather than a run of anything in the set.
		if HasNegationCue("the memory is now also obsolete") {
			t.Error(`HasNegationCue("the memory is now also obsolete") = true: two closed-set words inside one cue is beyond the allowance`)
		}

		// And through to a verdict, on the fingerprint arm: the same closed-set
		// word, with the memory's own wording beside the cue.
		s := newTestSignals(t)
		s.AddProse("the opencode plugin materializes its transcript under mkdtemp is now obsolete")
		if !s.contradicts(memTokens(t), "") {
			t.Error("a cue split by a closed-set word did not bind to the memory's own wording beside it")
		}
	})

	t.Run("a gap the closed set does not reach", func(t *testing.T) {
		cases := []struct {
			name  string
			prose string
		}{
			{
				// #858's sentence, unchanged. Two ordinary words stand between the
				// id and the cue and neither is in the set, so the widening does not
				// reach it.
				name:  "a subject clause between the id and the cue",
				prose: "Per " + id + ", I'll ignore the formatting.",
			},
			{
				// The same sentence at the width of the new allowance: the cue is
				// bound to "formatter", so the gap between the cue and the id is two
				// words long and one of them is not in the set.
				name:  "a determiner and a word the set does not name",
				prose: "ignore the formatter, " + id + " covers the build",
			},
			{
				// One word past the allowance, every one of them in the set. This is
				// the case that pins the constant: a set is not an unlimited run.
				name:  "one word past the allowance",
				prose: "ignore the memory note in " + id,
			},
			{
				// And the far side of the same edge: a gap inside the allowance that
				// contains a word outside it is not an allowance at all.
				name:  "inside the allowance, with a word outside the set",
				prose: "ignore the linter advice in " + id,
			},
			{
				// A subject clause is still a subject clause when a filler sits in
				// it, which is what keeps this from being "the gap is a filler's
				// excuse to reach anything nearby".
				name:  "a filler and a subject between the id and the cue",
				prose: "Per the " + id + ", I'll ignore the formatting.",
			},
			{
				// THE SAME SENTENCE WITHOUT THE CONTRACTION, and the case the closed
				// set opens on this side. "I'll" tokenises to two words and was never
				// adjacent to the cue, so the pre-existing fixture passed the gap for a
				// reason that had nothing to do with the rule: the words between the id
				// and the cue were "i" and "ll", and neither is a filler. "I" is ONE
				// word, so this sentence has exactly one word between, and whether it
				// binds turns entirely on "now" — which is in the set, so nothing may
				// consume it before the cue and shorten the measured distance.
				name:  "a filler beside the cue and a subject before it",
				prose: "Per " + id + ", I now ignore the formatting.",
			},
			{
				// The same reach from the other side of the cue, and the sharpest form
				// of it: a filler sits IMMEDIATELY before the cue, one word after a
				// word the set does not name. Absorbing that filler moves the span's
				// leading edge onto "memory", the gap becomes just "my", and this
				// binds — which is why the rule is that nothing may be consumed ahead
				// of a cue's first word, not merely that the set is small.
				name:  "a filler immediately before the cue, past a word outside the set",
				prose: "Per " + id + ", my memory is obsolete.",
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				s := newTestSignals(t)
				s.AddProse(tc.prose)
				if s.contradicts(testTokens(unshared), id) {
					t.Errorf("contradicts = true for %q: the gap between the cue and the id is not the closed set", tc.prose)
				}
			})
		}
	})
}

// TestANAdjacentFillerIsStillApartOfTheDistance: cueRun tolerates closed-set words
// BETWEEN a cue's own words, and it must not reach round the FRONT of a cue. A
// filler ahead of a cue is part of the distance the cue's object is measured over,
// so consuming it moves the span's leading edge onto the filler and both consumers
// of a span then read from the wrong position:
//
//   - boundToCue measures the gap to c.start, so a consumed filler shortens the gap
//     by one word and an agreeing sentence binds. "Per <id>, I now ignore the
//     formatting" is #858's own sentence with a contraction expanded, and it came
//     back contradicted.
//   - insideCue reports the filler as cue text, so boundPositions' sideward skip
//     steps over it — and when the memory's own word IS that filler, the binding no
//     longer reaches anything the memory holds.
//
// The second is the shape the closed set exists for: a memory written in Ghost's
// own vocabulary, whose distinctive word sits beside the cue. That denial quotes
// the memory and clears the token bar by itself, so the binding is the only thing
// that can file it, and a filler absorbed into the cue stops exactly that. It is
// asserted as a POSITIVE because it is one — the right answer is to deny it — and
// it is kept here rather than filed as its own bug because both halves are the one
// mechanism: a fix for either alone must not be allowed to look like a fix for the
// other.
//
// The first half is checked on the SPANS rather than on a verdict, because that is
// the mechanism and a verdict can be reached by the other arm: each of these has
// exactly one cue, and it is on the cue's own first word.
func TestANAdjacentFillerIsStillApartOfTheDistance(t *testing.T) {
	const id = "4F3A9C1E7B2D8A6F5C0E1234AB5678EF"
	unshared := "a memory whose wording the agent never repeated"

	t.Run("the span starts on the cue, not on the filler", func(t *testing.T) {
		cases := []struct {
			prose string
			want  []cueSpan
		}{
			// A single-word cue behind a filler. Before the fix this produced TWO
			// spans, one of them starting on "now".
			{prose: "now ignore the formatting", want: []cueSpan{{start: 1, end: 1}}},
			// And a multi-word cue behind one, which is the shape that swallowed the
			// memory's own word.
			{prose: "in is obsolete", want: []cueSpan{{start: 1, end: 2}}},
			{prose: "the note is stale", want: []cueSpan{{start: 2, end: 3}}},
		}
		for _, tc := range cases {
			got := cueSpans(splitWords(tc.prose))
			if !slices.Equal(got, tc.want) {
				t.Errorf("cueSpans(%q) = %v, want %v: the span's leading edge is the cue's first word, never a filler ahead of it",
					tc.prose, got, tc.want)
			}
		}
	})

	t.Run("and it does not shorten the gap", func(t *testing.T) {
		// #858's own sentence with the contraction expanded. One word stands
		// between the id and the cue and it is not a filler, so this is unbound.
		s := newTestSignals(t)
		s.AddProse("Per " + id + ", I now ignore the formatting.")
		if s.contradicts(testTokens(unshared), id) {
			t.Error("a filler before the cue was consumed, shortening the gap to one word and filing agreement as a contradiction")
		}
	})

	t.Run("nor hide the word the memory's own wording supplies", func(t *testing.T) {
		// A memory written in Ghost's own vocabulary, so the word beside the cue IS
		// one of the memory's tokens. The denial quotes the memory and clears the
		// token bar on its own; the binding is what carries it.
		mem := "the entry that records weekly releases"
		toks := testTokens(mem)

		s := newTestSignals(t)
		s.AddProse("that entry is obsolete, we discussed weekly releases earlier")
		if !s.matches(toks) {
			t.Fatal("the fixture does not clear the token arm's bar, so it proves nothing about the binding")
		}
		if !s.contradicts(toks, "") {
			t.Error("the memory's own word beside the cue was stepped over, so a genuine denial of it was not filed")
		}
	})
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
