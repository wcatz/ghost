package audit

import (
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
