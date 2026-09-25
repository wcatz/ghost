package memory

import (
	"golang.org/x/text/unicode/norm"
	"strings"
	"unicode"
)

// foldOnlyEquivalent reports whether two texts are the same memory.
//
// This is an EQUALITY test, and it is deliberately not a similarity one. FoldOnly
// drops the incoming text: keeping only one of the pair has to lose nothing, and
// any scoring function that tolerates a difference has a shape that difference
// can take which reverses the meaning while the score stays high. "use sqlite
// not postgres" and "use postgres not sqlite" are the shortest known example —
// one word apart, and opposite — but at twenty words the failure is easier to
// miss, not harder: a long instruction differing only in "is required" versus
// "is optional" is the same memory to a bag of words and a contradiction to
// whoever has to follow it.
//
// So the rule is: equal after normalization, or not folded. Anything else is
// stored as its own row. A redundant row costs a window slot and some
// resolve/supersede work; dropping the opposite of what was promoted loses
// information that nothing in the database can reconstruct. Only one of those is
// worth the risk.
//
// Normalization, in order:
//
//   - NFKC, so a compatibility character (a fullwidth letter, a ligature) is
//     the character it renders as rather than a separate one;
//   - lowercase, since case is presentation;
//   - U+2019 and U+02BC mapped to the ASCII apostrophe, because those are the
//     same character in a different keyboard layout and the apostrophe is what
//     separates a contraction from the word before it;
//   - every Unicode punctuation and symbol character removed, since "don't" and
//     "dont" differ only by a mark that carries no meaning here;
//   - whitespace collapsed to single spaces and trimmed.
//
// Letters, digits and everything else are kept, so a difference in any of them
// survives to be compared.
func foldOnlyEquivalent(a, b string) bool {
	return normalizeForCompare(a) == normalizeForCompare(b)
}

// normalizeForCompare renders two texts into the form the equality test
// compares. Anything removed here is a difference the test is blind to, so each
// removal has to be one that cannot carry meaning.
func normalizeForCompare(s string) string {
	s = strings.ToLower(norm.NFKC.String(s))
	// The typographic apostrophe, the modifier letter apostrophe, and the ASCII
	// one are one character to a reader. Left alone, "don't" and "don’t" would
	// normalize to different strings and never fold, which is precisely the
	// re-punctuation this is meant to be blind to.
	s = strings.NewReplacer("’", "'", "ʼ", "'").Replace(s)

	var b strings.Builder
	b.Grow(len(s))
	pendingSpace := true // also strips leading whitespace
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			if !pendingSpace {
				b.WriteByte(' ')
				pendingSpace = true
			}
		case unicode.IsPunct(r), unicode.IsSymbol(r):
			// Dropped entirely: a mark between two words cannot change what
			// the words say. Dropping it also means "state-of-the-art" and
			// "state of the art" compare equal, which is the intent.
		default:
			b.WriteRune(r)
			pendingSpace = false
		}
	}
	return strings.TrimSpace(b.String())
}
