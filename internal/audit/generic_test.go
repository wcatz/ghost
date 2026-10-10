package audit

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// fillerWords is n distinct made-up words, none of them in any fixture memory.
func fillerWords(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString("zqxv")
		for v := i + 1; v > 0; v /= 26 {
			b.WriteByte(byte('a' + v%26))
		}
		b.WriteByte(' ')
	}
	return b.String()
}

// turnsAt scripts one turn per text, a minute apart, and returns the instant of
// the first. An unrelated word is added to each so no two turns are the same set.
func turnsAt(s *Signals, texts []string) time.Time {
	start := time.Now().Add(-2 * time.Hour)
	for i, text := range texts {
		s.SetAt(start.Add(time.Duration(i) * time.Minute))
		s.AddProse(text)
	}
	return start
}

func repeatTurns(n int, text string) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s step%dx", text, i)
	}
	return out
}

// TestATurnOfAFileIsNotRead pins the outlier line from both sides: a turn of 149
// distinct tokens that holds the whole memory is a use, and one of 150 is not read.
func TestATurnOfAFileIsNotRead(t *testing.T) {
	for _, tc := range []struct {
		distinct int
		want     Outcome
	}{
		{outlierTurnTokens - 1, OutcomeUsed},
		{outlierTurnTokens, OutcomeIgnored},
	} {
		s := NewWithHasher(testHasher)
		extra := tc.distinct - len(memWords)
		turnsAt(s, []string{memContent + " " + fillerWords(extra)})
		if got := len(s.turns[0].fps); got != tc.distinct {
			t.Fatalf("fixture turn holds %d distinct tokens, want %d", got, tc.distinct)
		}
		if v := compareOne(t, s, memContent); v.Outcome != tc.want {
			t.Errorf("a turn of %d distinct tokens judged %s, want %s", tc.distinct, v.Outcome, tc.want)
		}
	}
}

// TestATurnThatGrowsPastTheLineIsNotRead: a scanner steps the instant once per line
// and adds every block of the line under it, so a turn can be small when it opens and
// large when it closes. The size that counts is the closed turn's.
func TestATurnThatGrowsPastTheLineIsNotRead(t *testing.T) {
	s := NewWithHasher(testHasher)
	s.SetAt(time.Now().Add(-time.Hour))
	s.AddProse(memContent)
	if v := compareOne(t, s, memContent); v.Outcome != OutcomeUsed {
		t.Fatalf("the memory alone in a turn judged %s, want used", v.Outcome)
	}
	s.AddToolArgs(fillerWords(outlierTurnTokens))
	if len(s.turns) != 1 {
		t.Fatalf("%d turns, want the one the instant opened", len(s.turns))
	}
	if v := compareOne(t, s, memContent); v.Outcome != OutcomeIgnored {
		t.Errorf("a turn that grew to %d distinct tokens judged %s, want ignored", len(s.turns[0].fps), v.Outcome)
	}
	// Nothing was dropped from storage: the scan is still not empty and still ordered.
	if s.Empty() || !s.Ordered() {
		t.Error("the outlier turn was dropped from the scan, so Empty/Ordered changed")
	}
}

// TestTheOutlierRuleSurvivesTheSidecar: the rule reads the turn's fingerprints, which
// is what the sidecar carries, so a scan read back is judged as the one written was.
func TestTheOutlierRuleSurvivesTheSidecar(t *testing.T) {
	s := NewWithHasher(testHasher)
	s.SetSessionID(testSession)
	turnsAt(s, []string{memContent + " " + fillerWords(outlierTurnTokens)})
	path, err := WriteSidecar(t.TempDir(), s)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ReadSidecar(path, testHasher)
	if err != nil {
		t.Fatal(err)
	}
	if v := compareOne(t, back, memContent); v.Outcome != OutcomeIgnored {
		t.Errorf("after the sidecar the outlier turn judged %s, want ignored", v.Outcome)
	}
}

// genericSession is twelve turns, the first `common` of which hold the three words
// opencode, plugin and transcript (and a step marker so the turns differ).
func genericSession(common int) *Signals {
	s := NewWithHasher(testHasher)
	texts := make([]string, 0, 13)
	for i := 0; i < 12; i++ {
		text := fmt.Sprintf("working on step%dx of the change", i)
		if i < common {
			text += " opencode plugin transcript"
		}
		texts = append(texts, text)
	}
	turnsAt(s, texts)
	return s
}

// TestAWordInAQuarterOfTheTurnsIsGeneric pins the line from both sides, over the
// smallest session the rule applies to: three of twelve turns is a quarter, two is not.
func TestAWordInAQuarterOfTheTurnsIsGeneric(t *testing.T) {
	for _, tc := range []struct {
		common  int
		generic bool
	}{{3, true}, {2, false}} {
		s := genericSession(tc.common)
		got := genericFingerprints(s.turns)
		if has := got[testHasher.Fingerprint("transcript")]; has != tc.generic {
			t.Errorf("a word in %d of 12 turns: generic = %v, want %v", tc.common, has, tc.generic)
		}
	}
	// A short session has no vocabulary to call common, whatever it repeats.
	short := NewWithHasher(testHasher)
	turnsAt(short, repeatTurns(genericMinTurns-1, "opencode plugin transcript"))
	if g := genericFingerprints(short.turns); len(g) != 0 || g == nil {
		t.Errorf("a session of %d turns has generic words %v, want none (and a non-nil set)", genericMinTurns-1, g)
	}
}

// TestGenericWordsDoNotMakeAUse: three of the memory's words are in every turn of the
// session, and one later turn carries those three and two more. Counting them it clears
// (5 of 8); without them it holds two words, which is not a use.
func TestGenericWordsDoNotMakeAUse(t *testing.T) {
	s := NewWithHasher(testHasher)
	texts := repeatTurns(12, "opencode plugin transcript")
	texts = append(texts, "opencode plugin transcript mkdtemp directory")
	turnsAt(s, texts)
	if v := compareOne(t, s, memContent); v.Outcome != OutcomeIgnored {
		t.Errorf("generic words made a use: %s", v.Outcome)
	}

	// The same last turn in a session where those words are not common is a use, so
	// the case above is the generic rule's doing and not the bar's.
	plain := NewWithHasher(testHasher)
	turnsAt(plain, append(repeatTurns(12, "unrelated filler text"), "opencode plugin transcript mkdtemp directory"))
	if v := compareOne(t, plain, memContent); v.Outcome != OutcomeUsed {
		t.Fatalf("the control judged %s, want used", v.Outcome)
	}
}

// TestGenericWordsLeaveTheNumeratorOnly: the memory's total is whole. Three generic
// words and three specific ones are six of the memory's eight; that is under half. A
// bar over the five words left would be met by three.
func TestGenericWordsLeaveTheNumeratorOnly(t *testing.T) {
	s := NewWithHasher(testHasher)
	texts := repeatTurns(12, "opencode plugin transcript")
	texts = append(texts, "opencode plugin transcript mkdtemp directory hook")
	turnsAt(s, texts)
	if v := compareOne(t, s, memContent); v.Outcome != OutcomeIgnored {
		t.Errorf("3 specific words of 8 judged %s: the generic words shrank the memory's total", v.Outcome)
	}
	s2 := NewWithHasher(testHasher)
	turnsAt(s2, append(repeatTurns(12, "opencode plugin transcript"), "opencode plugin transcript mkdtemp directory hook close"))
	if v := compareOne(t, s2, memContent); v.Outcome != OutcomeUsed {
		t.Errorf("4 specific words of 8 judged %s, want used: half of the memory is still a use", v.Outcome)
	}
}

// TestAViewKeepsTheWholeSessionsVocabulary: Run judges a view of the turns after a
// call, and the words common to the SESSION are not the words common to that view.
func TestAViewKeepsTheWholeSessionsVocabulary(t *testing.T) {
	s := NewWithHasher(testHasher)
	texts := repeatTurns(12, "opencode plugin transcript")
	start := turnsAt(s, append(texts, "opencode plugin transcript mkdtemp directory"))
	cutoff := start.Add(12*time.Minute - time.Second)
	view := s.Since(cutoff)
	if len(view.turns) != 1 {
		t.Fatalf("the view holds %d turns, want only the last", len(view.turns))
	}
	if v := compareOne(t, view, memContent); v.Outcome != OutcomeIgnored {
		t.Errorf("the view judged %s: its one turn was its whole vocabulary", v.Outcome)
	}
}

// TestComparingDoesNotWriteToTheSignals: Compare is read-only on what it is handed.
func TestComparingDoesNotWriteToTheSignals(t *testing.T) {
	s := genericSession(3)
	before := len(s.turns)
	compareOne(t, s, memContent)
	if s.generic != nil || len(s.turns) != before {
		t.Error("Compare wrote to the Signals it was given")
	}
}

// TestGenericWordsAreDerivedAfterTheSidecar: nothing about them is stored, so a scan
// read back judges as the one written did.
func TestGenericWordsAreDerivedAfterTheSidecar(t *testing.T) {
	s := NewWithHasher(testHasher)
	s.SetSessionID(testSession)
	texts := repeatTurns(12, "opencode plugin transcript")
	turnsAt(s, append(texts, "opencode plugin transcript mkdtemp directory"))
	written, err := WriteSidecar(t.TempDir(), s)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ReadSidecar(written, testHasher)
	if err != nil {
		t.Fatal(err)
	}
	for name, sig := range map[string]*Signals{"written": s, "read back": back} {
		if v := compareOne(t, sig, memContent); v.Outcome != OutcomeIgnored {
			t.Errorf("%s: judged %s, want ignored", name, v.Outcome)
		}
	}
}

// TestGenericWordsDoNotSoftenAContradiction: a denial is judged on its own bar, over
// the memory's whole wording, however common the words are in the session.
func TestGenericWordsDoNotSoftenAContradiction(t *testing.T) {
	s := NewWithHasher(testHasher)
	s.SetSessionID(testSession)
	start := time.Now().Add(-2 * time.Hour)
	for i := 0; i < 12; i++ {
		s.SetAt(start.Add(time.Duration(i) * time.Minute))
		s.AddProse(fmt.Sprintf("step%dx opencode plugin materializes transcript mkdtemp directory hook close", i))
	}
	s.SetAt(start.Add(13 * time.Minute))
	s.AddProse("that is wrong: the opencode plugin materializes its transcript under mkdtemp and the directory hook close")
	if v := compareOne(t, s, memContent); v.Outcome != OutcomeContradicted {
		t.Errorf("verdict %s, want contradicted: the generic rule reaches the used arm only", v.Outcome)
	}
}

// TestASaveOrUpdateDoesNotCiteWhatItNames: the id in a write tool's arguments is the
// memory being rewritten. Its words are still read as a restatement.
func TestASaveOrUpdateDoesNotCiteWhatItNames(t *testing.T) {
	s := newTestSignals(t)
	s.AddSaveArgs(testMemoryID + " " + memContent)
	if s.HasID(testMemoryID) {
		t.Error("an id in a save's arguments was recorded as a citation")
	}
	v := compareOneAs(t, s, testMemoryID, memContent)
	if v.Outcome != OutcomeSuperseded {
		t.Errorf("a save restating the memory judged %s, want superseded", v.Outcome)
	}

	// The control: the same id in any other tool call, or in prose, is a citation.
	s2 := newTestSignals(t)
	s2.AddToolArgs("ghost_memory_search --note " + testMemoryID)
	if !s2.HasID(testMemoryID) {
		t.Error("an id in an ordinary tool call stopped being a citation")
	}
}

// TestAGiantTurnDoesNotMakeWordsCommon: the frequency is over the turns a person
// wrote. Two outlier turns holding a word would lift it from 2 of 12 to 4 of 14.
func TestAGiantTurnDoesNotMakeWordsCommon(t *testing.T) {
	s := NewWithHasher(testHasher)
	texts := make([]string, 0, 14)
	for i := 0; i < 12; i++ {
		text := fmt.Sprintf("working on step%dx of the change", i)
		if i < 2 {
			text += " transcript"
		}
		texts = append(texts, text)
	}
	giant := "transcript " + fillerWords(outlierTurnTokens)
	texts = append(texts, giant, giant+" again")
	turnsAt(s, texts)
	if genericFingerprints(s.turns)[testHasher.Fingerprint("transcript")] {
		t.Error("a word in 2 of 12 ordinary turns became generic through two outlier turns")
	}
}

// TestTheGenericLineIsPinnedAtItsEdges pins both constants where a change to either
// moves a verdict: a word in 25 of 100 turns is generic and in 24 is not (a quarter,
// not a fifth), and the same word in a session one turn under the minimum is not
// generic at all (so the minimum is twelve).
func TestTheGenericLineIsPinnedAtItsEdges(t *testing.T) {
	withWord := func(turns, holding int) map[string]bool {
		s := NewWithHasher(testHasher)
		texts := make([]string, turns)
		for i := range texts {
			texts[i] = fmt.Sprintf("working on step%dx of the change", i)
			if i < holding {
				texts[i] += " transcript"
			}
		}
		turnsAt(s, texts)
		return genericFingerprints(s.turns)
	}
	fp := testHasher.Fingerprint("transcript")
	if !withWord(100, 25)[fp] {
		t.Error("a word in 25 of 100 turns is not generic: the share is no longer a quarter")
	}
	if withWord(100, 24)[fp] {
		t.Error("a word in 24 of 100 turns is generic: the share is looser than a quarter")
	}
	if !withWord(genericMinTurns, 3)[fp] {
		t.Errorf("a word in 3 of %d turns is not generic", genericMinTurns)
	}
	// 11 turns, the word in 3 of them (27%): over the share, under the minimum.
	if withWord(11, 3)[fp] {
		t.Error("a session of 11 turns has generic words: the minimum is below twelve")
	}
	if genericMinTurns != 12 || genericShare != 4 {
		t.Errorf("constants moved to %d and %d: the documented rule is twelve turns and a quarter", genericMinTurns, genericShare)
	}
}

// TestGenericWordsDoNotMakeASupersede: a save whose words are the session's own
// vocabulary does not file a memory superseded, for the same reason prose does not
// file it used. The save arm reads the same generic set.
func TestGenericWordsDoNotMakeASupersede(t *testing.T) {
	build := func(common bool) *Signals {
		s := NewWithHasher(testHasher)
		texts := repeatTurns(12, "unrelated filler text")
		if common {
			texts = repeatTurns(12, "opencode plugin transcript")
		}
		start := turnsAt(s, texts)
		s.SetAt(start.Add(30 * time.Minute))
		s.AddSaveArgs("opencode plugin transcript mkdtemp directory")
		return s
	}
	if v := compareOne(t, build(false), memContent); v.Outcome != OutcomeSuperseded {
		t.Fatalf("the control judged %s, want superseded", v.Outcome)
	}
	if v := compareOne(t, build(true), memContent); v.Outcome != OutcomeIgnored {
		t.Errorf("generic words in a save judged %s, want ignored", v.Outcome)
	}
}
