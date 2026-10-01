package audit

import "testing"

// compareOne runs Compare over a single judged memory and returns its verdict,
// so each case below reads as one sentence about one bucket.
//
// The memory is named "MEM1", which is deliberately NOT id-shaped: every case
// here that is not ABOUT the identifier arm must be judged by wording alone, and
// an id-shaped name would let a stray "MEM1" in some fixture's prose satisfy the
// id arm and pass a test for a different reason than the one it names. The two
// cases that ARE about the arm call compareOneAs with testMemoryID.
func compareOne(t *testing.T, s *Signals, content string) Verdict {
	t.Helper()
	return compareOneAs(t, s, "MEM1", content)
}

// compareOneAs is compareOne for a named memory, which the identifier arms need
// because they match the judged memory's id against the one the agent spoke.
func compareOneAs(t *testing.T, s *Signals, memoryID, content string) Verdict {
	t.Helper()
	vs := Compare(s, []Judged{{MemoryID: memoryID, Content: content}})
	if len(vs) != 1 {
		t.Fatalf("Compare returned %d verdicts, want 1", len(vs))
	}
	return vs[0]
}

func TestCompareUsedByIdentifier(t *testing.T) {
	s := newTestSignals(t)
	s.AddProse("the transcript is documented on MEM1") // not id-shaped
	// The real id, named by the agent while saying nothing about the wording.
	s.AddProse("applied the fix from " + testMemoryID)

	got := compareOneAs(t, s, testMemoryID, memContent)
	if got.Outcome != OutcomeUsed || got.Signal != SignalIdentifier {
		t.Errorf("verdict = %+v, want used/%s", got, SignalIdentifier)
	}
}

func TestCompareUsedByTokenOverlap(t *testing.T) {
	s := newTestSignals(t)
	s.AddProse("remember that the opencode plugin materializes its transcript under mkdtemp")

	got := compareOne(t, s, memContent)
	if got.Outcome != OutcomeUsed || got.Signal != SignalToken {
		t.Errorf("verdict = %+v, want used/%s", got, SignalToken)
	}
}

// TestCompareIgnoreIsNotAUsefulnessScore is the bucket's own contract: "ignored"
// says the agent's own words never mentioned the memory, and nothing about
// whether the memory was any good. The signal column is empty in both cases.
func TestCompareIgnoreIsNotAUsefulnessScore(t *testing.T) {
	s := newTestSignals(t)
	s.AddProse("refactored the loader and moved on")

	got := compareOne(t, s, memContent)
	if got.Outcome != OutcomeIgnored {
		t.Errorf("outcome = %q, want %q", got.Outcome, OutcomeIgnored)
	}
	if got.Signal != "" {
		t.Errorf("signal = %q on an ignored verdict, want empty: the column records what PROVED a use", got.Signal)
	}
}

// TestCompareSupersededIsSeparateFromContradicted pins the split the issue
// asked for: an in-session save that restates the memory is its own bucket, and
// is not counted as either a use or a contradiction.
func TestCompareSupersededIsSeparateFromContradicted(t *testing.T) {
	s := newTestSignals(t)
	s.AddSaveArgs("learned this session: " + memContent)

	got := compareOne(t, s, memContent)
	if got.Outcome != OutcomeSuperseded {
		t.Errorf("outcome = %q, want %q", got.Outcome, OutcomeSuperseded)
	}
	if got.Signal != "" {
		t.Errorf("signal = %q, want empty: no positive evidence of a use was found", got.Signal)
	}
}

func TestCompareContradictedNeedsAnExplicitNegation(t *testing.T) {
	s := newTestSignals(t)
	s.AddProse("note that the opencode plugin materializes its transcript under mkdtemp")

	got := compareOne(t, s, memContent)
	if got.Outcome != OutcomeUsed {
		t.Fatalf("outcome = %q, want %q before the negation is introduced", got.Outcome, OutcomeUsed)
	}

	s.AddProse("that is wrong — the opencode plugin materializes its transcript under mkdtemp is no longer true")
	got = compareOne(t, s, memContent)
	if got.Outcome != OutcomeContradicted {
		t.Errorf("outcome = %q, want %q", got.Outcome, OutcomeContradicted)
	}
}

// TestCompareContradictedOutranksAUse: a session that both used a memory and
// then said it was wrong has the more urgent finding on it, and an audit that
// filed the use first would bury the one an operator has to act on.
func TestCompareContradictedOutranksAUse(t *testing.T) {
	s := newTestSignals(t)
	s.AddProse("starting from the opencode plugin materializes its transcript under mkdtemp")
	s.AddProse("on reflection the opencode plugin materializes its transcript under mkdtemp is not true any more")

	got := compareOne(t, s, memContent)
	if got.Outcome != OutcomeContradicted {
		t.Errorf("outcome = %q, want %q", got.Outcome, OutcomeContradicted)
	}
}

// TestCompareContradictedByIdentifier: the negation arm reads the id as well as
// the wording, so a sentence that renames a memory in the negative is caught
// without any token overlap at all.
func TestCompareContradictedByIdentifier(t *testing.T) {
	s := newTestSignals(t)
	s.AddProse("ignore " + testMemoryID + ", that guidance is obsolete")

	got := compareOneAs(t, s, testMemoryID, "a memory whose wording the agent never repeated")
	if got.Outcome != OutcomeContradicted {
		t.Errorf("outcome = %q, want %q", got.Outcome, OutcomeContradicted)
	}
}

// TestCompareTokenArmNeedsEnoughOfTheMemory is the bar that keeps `used` honest:
// two coincidental words in a long transcript are not evidence that a memory
// was read.
func TestCompareTokenArmNeedsEnoughOfTheMemory(t *testing.T) {
	toks := memTokens(t)
	s := newTestSignals(t)
	// Exactly two of the memory's own words, and nothing else.
	s.AddProse(memWords[0] + " " + memWords[1])
	if s.matches(toks) {
		t.Fatal("two matching fingerprints satisfied the token arm")
	}

	s.AddProse("and " + memWords[2])
	if !s.matches(toks) {
		t.Error("three matching fingerprints did not satisfy the token arm")
	}
}

// TestCompareTokenArmNeedsEveryTokenOfAShortMemory: the arm asks for a THIRD of a
// memory's distinctive words and never fewer than three, so for a memory of
// three that is all three. A longer memory can clear the bar with a fraction —
// which is the case TestCompareTokenArmNeedsEnoughOfTheMemory covers.
func TestCompareTokenArmNeedsEveryTokenOfAShortMemory(t *testing.T) {
	toks := testTokens("alpine meadow protocol")
	if len(toks) != 3 {
		t.Fatalf("fixture = %d tokens, want 3", len(toks))
	}
	s := newTestSignals(t)
	s.AddProse("alpine meadow")
	if s.matches(toks) {
		t.Error("two of three words satisfied the token arm")
	}
	s.AddProse("protocol")
	if !s.matches(toks) {
		t.Error("three of three words did not satisfy the token arm")
	}
}

// TestCompareAMemoryTooShortToMatchIsNeverUsedByToken: "Use Postgres everywhere"
// has TWO distinctive tokens — "use" is under the four-character floor — which
// is below the arm's floor of three however much of it the agent repeats. The
// token arm cannot claim such a memory at all, so it falls through to whatever
// the id and negation arms found.
func TestCompareAMemoryTooShortToMatchIsNeverUsedByToken(t *testing.T) {
	s := newTestSignals(t)
	s.AddProse("use postgres everywhere")

	got := compareOne(t, s, "Use Postgres everywhere")
	if got.Outcome != OutcomeIgnored {
		t.Errorf("outcome = %q, want %q: three distinctive tokens is below the arm's floor", got.Outcome, OutcomeIgnored)
	}
}

func TestCompareEmptyContentIsIgnored(t *testing.T) {
	s := newTestSignals(t)
	s.AddProse("the opencode plugin materializes its transcript under mkdtemp")

	got := compareOne(t, s, "")
	if got.Outcome != OutcomeIgnored {
		t.Errorf("outcome = %q, want %q for a memory with no content to match on", got.Outcome, OutcomeIgnored)
	}
}

// TestCompareIsPerMemory: one transcript carries many verdicts, and a memory the
// agent restated must not drag its neighbours to `used` with it.
func TestCompareIsPerMemory(t *testing.T) {
	s := newTestSignals(t)
	s.AddProse("the opencode plugin materializes its transcript under mkdtemp")

	vs := Compare(s, []Judged{
		{MemoryID: "A", Content: memContent},
		{MemoryID: "B", Content: "Pinned versions come from the lockfile, never from a floating tag"},
		{MemoryID: "C", Content: memContent},
	})
	if len(vs) != 3 {
		t.Fatalf("Compare returned %d verdicts, want one per judged memory", len(vs))
	}
	want := map[string]Outcome{"A": OutcomeUsed, "B": OutcomeIgnored, "C": OutcomeUsed}
	for _, v := range vs {
		if want[v.MemoryID] != v.Outcome {
			t.Errorf("verdict for %s = %q, want %q", v.MemoryID, v.Outcome, want[v.MemoryID])
		}
	}
}

// TestJudgedCarriesNoRoomForText pins the constraint the report rests on: what
// reaches the store from a comparison is an id, an outcome and a signal.
func TestJudgedCarriesNoRoomForText(t *testing.T) {
	j := Judged{MemoryID: "A", Content: memContent}
	if j.MemoryID == j.Content {
		t.Fatal("the fixture is wrong: MemoryID and Content must be distinct fields")
	}
	// A verdict is the only thing the comparison emits; it has no content field.
	v := Verdict{MemoryID: "A", Outcome: OutcomeUsed, Signal: SignalIdentifier}
	if v.MemoryID == "" || v.Outcome == "" {
		t.Error("a verdict must carry the memory it is about and the bucket it fell in")
	}
}
