package memory

import "testing"

// TestFoldOnlyEquivalentTable is the architect's ruling, case by case.
//
// FoldOnly DROPS the incoming text, so the question it has to answer is not
// "are these related" but "are these the same claim". Every pair below shares
// most of its words with its partner and means the OPPOSITE thing. A bag of
// words cannot tell them apart — that is the whole failure — so the comparison
// is order-sensitive, and a difference in whether the text negates anything is
// disqualifying on its own.
func TestFoldOnlyEquivalentTable(t *testing.T) {
	mustNotFold := []struct{ name, a, b string }{
		{
			"reordered negation",
			"use sqlite not postgres for storage",
			"use postgres not sqlite for storage",
		},
		{
			"reordered preference",
			"prefer tabs over spaces",
			"prefer spaces over tabs",
		},
		{
			"reordered steps",
			"run helmfile diff before helmfile apply",
			"run helmfile apply before helmfile diff",
		},
		{
			"contraction introduced",
			"don't commit directly to the main branch ever",
			"commit directly to the main branch ever",
		},
		{
			"contraction removed",
			"you shouldn't run live tests",
			"you should run live tests",
		},
		{
			"negation introduced",
			"the user is wayne",
			"the user isn't wayne",
		},
	}
	for _, c := range mustNotFold {
		t.Run("not fold/"+c.name, func(t *testing.T) {
			if foldOnlyEquivalent(c.a, c.b) {
				t.Errorf("foldOnlyEquivalent(%q, %q) = true, want false — these state opposite claims", c.a, c.b)
			}
		})
	}

	// The positive control. Without it, a rule that refuses everything would
	// pass every case above while reintroducing the redundancy this option
	// exists to remove.
	mustFold := []struct{ name, a, b string }{
		{
			"same words, different case",
			"Run The Linter Before Pushing",
			"run the linter before pushing",
		},
		{
			"same words, extra whitespace",
			"run  the linter   before pushing",
			"run the linter before pushing",
		},
		{
			"same words, punctuation",
			"run the linter, before pushing.",
			"run the linter before pushing",
		},
	}
	for _, c := range mustFold {
		t.Run("fold/"+c.name, func(t *testing.T) {
			if !foldOnlyEquivalent(c.a, c.b) {
				t.Errorf("foldOnlyEquivalent(%q, %q) = false, want true — the same claim in other words", c.a, c.b)
			}
		})
	}
}

// TestTokenizeContentKeepsContractionsAsNegation pins the tokenizer change. The
// apostrophe is a separator, so "don't" used to become "don" plus "t" — and "don"
// reads as nothing, leaving a negated instruction tokenized almost identically
// to its positive form. Every contraction must land on the same canonical
// negation token as a bare "not".
func TestTokenizeContentKeepsContractionsAsNegation(t *testing.T) {
	for _, s := range []string{
		"don't commit to main", "do not commit to main", "dont commit to main",
		"isn't ready", "is not ready", "shouldn't run live tests", "should not run live tests",
		"won't work", "will not work", "can't deploy", "cannot deploy", "doesn't matter",
		"never deploy on a friday", "no deploys on a friday", "avoid deploying on a friday",
	} {
		if !containsNegation(tokenizeContent(s)) {
			t.Errorf("tokenizeContent(%q) has no negation, want one in any spelling", s)
		}
	}
	// A positive sentence must not acquire one. "can" and "won" are in this
	// list deliberately: they are the fragments a split contraction leaves, and
	// both are ordinary English words, so a bare-fragment match would turn
	// "you can deploy" into a negation indistinguishable from "you cannot".
	for _, s := range []string{
		"run the linter before pushing", "the user is wayne", "commit directly to the main branch ever",
		"you should run live tests", "deploy on a tuesday", "the linter runs before pushing",
		"you can deploy on tuesday", "you can ship a release on friday", "we can run live tests",
	} {
		if containsNegation(tokenizeContent(s)) {
			t.Errorf("tokenizeContent(%q) invented a negation", s)
		}
	}
}
