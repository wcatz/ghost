package memory

import (
	"golang.org/x/text/unicode/norm"
	"strings"
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
// Normalization changes only what cannot change meaning, in order:
//
//   - NFKC, so a compatibility character (a fullwidth letter, a ligature) is
//     the character it renders as rather than a separate one;
//   - lowercase, since case is presentation;
//   - U+2019 and U+02BC mapped to the ASCII apostrophe, because those are the
//     same character typed on a different keyboard;
//   - whitespace collapsed to single spaces and trimmed;
//   - sentence punctuation (. , ; : ! ?) removed from the very END only.
//
// Every other character is kept, punctuation and symbols included. Inside a
// sentence a mark is meaning, not presentation: dropping them made
// "version >= 1.24" equal "version <= 1.24", "1.5 seconds" equal "15 seconds"
// and "C++" equal "C", and folding either pair loses the second memory. A pair
// that differs only in a mid-sentence mark ("state-of-the-art" / "state of the
// art") is therefore stored twice, which is the safe way to be wrong.
func foldOnlyEquivalent(a, b string) bool {
	return normalizeForCompare(a) == normalizeForCompare(b)
}

// normalizeForCompare renders a text into the form the equality test compares.
// Anything removed here is a difference the test is blind to, so each removal
// has to be one that cannot carry meaning.
func normalizeForCompare(s string) string {
	s = strings.ToLower(norm.NFKC.String(s))
	s = strings.NewReplacer("\u2019", "'", "\u02bc", "'").Replace(s)
	s = strings.Join(strings.Fields(s), " ")
	return strings.TrimRight(s, ".,;:!? ")
}
