package secret

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// The invisible FORMAT class — Unicode category Cf — and the detector's rule
// about it.
//
// A format character carries no width of its own and prints as nothing a
// reader can see, so a value that differs from another only by one of them
// reads as the other. The rules in this package matched the raw BYTES of a
// value, so a single invisible character placed inside a token-shaped value
// broke the match: the value was recognised as nothing, and every writer that
// answers from Detect stored it — replayed into every later session, mirrored
// to the vault and quoted into the next prompt.
//
// The rule here is that the value the detector judges is the value a READER
// sees. A zero-width space is invisible, so a token that differs from a real
// one only by an invisible character IS that token to whoever reads it.
//
// The bytes are judged as well, so the verdict is never weaker than it was. A
// rule that needs a word boundary reads the bytes, and the invisible character
// is not a word character, so the token beside it starts a word; removing the
// character joins the two and the boundary goes with it. A guard that stops
// recognising is indistinguishable from a guard that is not there.
//
// The predicate lives HERE rather than in internal/memory, where it was,
// because the dependency runs the other way: internal/memory imports this
// package to reach the detector, so a rule the detector needs cannot live
// above it. memory.IsFormatRune delegates to this one, so the renderers that
// escape the class, the write boundaries that refuse it and the detector that
// sees through it share ONE implementation of the rule.
//
// The class is the WHOLE of category Cf rather than a list of characters: a
// list holds only what somebody thought of, and a character a later Unicode
// version adds is covered without a change here.

// IsFormatRune reports whether r is a Unicode FORMAT character — category Cf —
// and so carries no width of its own and prints as nothing a reader can see.
//
// The lower bound is a fast path and not a second rule: the lowest FORMAT
// character is SOFT HYPHEN (U+00AD), and every code point below it is assigned
// and cannot become one.
func IsFormatRune(r rune) bool {
	if r < 0x00AD {
		return false
	}
	return unicode.In(r, unicode.Cf)
}

// isInvisibleForDetection reports whether r is a character that is invisible to
// a reader and that the detector must see THROUGH when it judges a value.
//
// It is broader than IsFormatRune (which stays exactly category Cf, because the
// renderers and the tag boundary escape or refuse only that class): a reader
// cannot see a combining mark, a variation selector, a Hangul filler, an
// object replacement character, a control character (all of category Cc except
// tab, newline and carriage return, which are visible line structure) or a
// braille blank either, and a token that differs from a real one only by one of
// them is that token to whoever reads it. Detection therefore builds its copy
// of the value with every one of these removed.
//
// The class is detection-only: it is never used to decide what a renderer
// escapes or what a write boundary refuses, because those surfaces answer a
// different question (what can a reader SEE in stored text) and the answer
// there is the whole of category Cf and nothing else.
func isInvisibleForDetection(r rune) bool {
	if IsFormatRune(r) {
		return true
	}
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) {
		return true
	}
	// All control characters (category Cc) except tab (\t), newline (\n) and
	// carriage return (\r), which are visible line structure. Also the specific
	// invisible characters that are not in Cc: the Hangul fillers, the object
	// replacement character, and the braille blank.
	if unicode.Is(unicode.Cc, r) && r != '\t' && r != '\n' && r != '\r' {
		return true
	}
	switch r {
	case 0x115F, 0x1160, 0x3164, 0xFFA0, 0xFFFC, 0x2800:
		return true
	}
	return false
}

// withoutInvisibleForDetection returns text with every character
// isInvisibleForDetection removes, and whether it removed any.
//
// What is left is the value as a reader sees it, which is the value the rules
// have to be judged against. Every other byte is copied verbatim, an invalid
// one included: it is not an invisible character, and this copy is the value the
// rules read.
//
// The text comes back unchanged when it holds none of those characters, so the
// common case costs one scan and no allocation.
func withoutInvisibleForDetection(text string) (string, bool) {
	first := -1
	for i, r := range text {
		if isInvisibleForDetection(r) {
			first = i
			break
		}
	}
	if first < 0 {
		return text, false
	}
	var b strings.Builder
	b.Grow(len(text))
	b.WriteString(text[:first])
	rest := text[first:]
	for len(rest) > 0 {
		r, size := utf8.DecodeRuneInString(rest)
		if isInvisibleForDetection(r) {
			rest = rest[size:]
			continue
		}
		if r == utf8.RuneError && size == 1 {
			b.WriteByte(rest[0])
		} else {
			b.WriteString(rest[:size])
		}
		rest = rest[size:]
	}
	return b.String(), true
}
