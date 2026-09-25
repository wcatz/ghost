package memory

import (
	"strings"
	"unicode/utf8"
)

// foldOnlyThreshold is the order-sensitive similarity a FoldOnly fold requires.
//
// It is deliberately far above the Jaccard bar the default fold uses. The
// default fold can afford to be generous because it keeps the caller's wording
// either way — the worst a false positive costs is a duplicate link. FoldOnly
// drops the incoming text, so the question is not "are these related" but "are
// these the same claim": "use sqlite not postgres" and "use postgres not
// sqlite" share eight of nine tokens and mean opposite things, and a set-based
// score cannot see the difference because it has thrown the order away.
const foldOnlyThreshold = 0.9

// negationTokens are the words that invert a claim. tokenizeContent folds every
// contraction into "not", so this set covers "don't", "isn't", "shouldn't",
// "won't", "can't" and "doesn't" as well as the bare words.
var negationTokens = map[string]bool{
	"never": true, "not": true, "no": true, "none": true, "avoid": true,
	"without": true, "disable": true, "disabled": true, "stop": true,
}

// foldOnlyEquivalent reports whether two texts are near-identical enough that
// keeping only one of them loses nothing.
//
// Two ways to qualify, either of which is enough:
//
//   - The normalized forms are equal. Normalizing lowercases, collapses runs of
//     whitespace, and reduces punctuation, so a re-cased or re-punctuated copy
//     of the same sentence compares equal. This is the shape most real
//     paraphrase noise is: the same sentence, typed differently.
//   - An order-sensitive similarity reaches foldOnlyThreshold AND the two texts
//     carry the same negation tokens. Order sensitivity is what separates
//     "use sqlite not postgres for storage" from "use postgres not sqlite for
//     storage": the words are the same, only their order differs, and the second
//     says the opposite thing. The negation check is the belt to that braces —
//     two texts that differ in whether they negate anything are not the same
//     claim at any similarity.
//
// Everything else is a near-match that is not a restatement, and the caller
// stores the incoming memory as its own row. Storing a redundant row is a
// problem; dropping the opposite of what was promoted is a data-loss bug, and
// only one of those two is worth the risk of a fold.
func foldOnlyEquivalent(a, b string) bool {
	if normalizeForCompare(a) == normalizeForCompare(b) {
		return true
	}
	if !sameNegationSet(a, b) {
		return false
	}
	return bigramJaccard(strings.Fields(normalizeForCompare(a)), strings.Fields(normalizeForCompare(b))) >= foldOnlyThreshold
}

// normalizeForCompare lowercases, collapses whitespace, and removes punctuation,
// so that differences in case, spacing, and punctuation are not differences in
// meaning.
func normalizeForCompare(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	lastSpace := true // also strips leading whitespace
	for _, r := range strings.ToLower(s) {
		if r < utf8.RuneSelf && !isASCIIAlnum(r) {
			if !lastSpace {
				b.WriteByte(' ')
				lastSpace = true
			}
			continue
		}
		b.WriteRune(r)
		lastSpace = false
	}
	return strings.TrimSpace(b.String())
}

// sameNegationSet reports whether both texts negate, or neither does. A
// difference in either direction is a difference in the claim.
// isASCIIAlnum reports whether r is an ASCII letter or digit, i.e. one of the
// characters normalization keeps.
func isASCIIAlnum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

func sameNegationSet(a, b string) bool {
	ta, tb := tokenizeContent(a), tokenizeContent(b)
	for token := range negationTokens {
		if ta[token] != tb[token] {
			return false
		}
	}
	return true
}

// bigramJaccard compares adjacent word PAIRS rather than a bag of words, so
// reordering the sentence changes the score. Two words swapped produce entirely
// different bigrams, which is the whole point: a set-based score sees them as
// identical and they are not.
func bigramJaccard(a, b []string) float64 {
	if len(a) < 2 || len(b) < 2 {
		// Too short to have bigrams: fall back to word-set Jaccard so a
		// one-word memory is not automatically un-foldable.
		setA, setB := map[string]bool{}, map[string]bool{}
		for _, w := range a {
			setA[w] = true
		}
		for _, w := range b {
			setB[w] = true
		}
		return jaccard(setA, setB)
	}
	ga, gb := make(map[string]bool, len(a)-1), make(map[string]bool, len(b)-1)
	for i := 0; i+1 < len(a); i++ {
		ga[a[i]+" "+a[i+1]] = true
	}
	for i := 0; i+1 < len(b); i++ {
		gb[b[i]+" "+b[i+1]] = true
	}
	return jaccard(ga, gb)
}
