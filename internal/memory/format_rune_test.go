package memory

// The FORMAT class, pinned EXACTLY.
//
// IsFormatRune is the ONE predicate that answers "is this rune invisible?" for
// the renderers that print stored text and the write boundaries that refuse
// it, so the class it names is the whole guarantee: a renderer that escapes the
// class shows a reader every invisible character a stored value carries, and a
// write boundary that refuses it keeps one out of the store in the first place.
//
// The assertion below enumerates the EXACT membership over EVERY code point
// rather than checking a table of hostile examples, because the failures are
// all in the characters nobody thought of. The bidi controls were escaped
// first and the rest of the category was not, so a tag of `gh` + U+200B +
// `ost` printed as `ghost` (#1649, #1651, #1663).
//
// The reference is the standard library's category TABLE, walked here, rather
// than a list written out in this file: a written list is the rule this
// replaced, because it holds only what somebody thought of. What the sweep adds
// over the implementation is the fast path the predicate takes below U+00AD,
// which is checked at every code point rather than trusted.

import (
	"testing"
	"unicode"
)

// formatCodePoints lists the members of Unicode category Cf from the standard
// library's tables, so the reference and the implementation are two different
// paths to the same list.
func formatCodePoints() map[rune]bool {
	out := map[rune]bool{}
	for _, rng := range unicode.Cf.R16 {
		for r := rune(rng.Lo); r <= rune(rng.Hi); r += rune(rng.Stride) {
			out[r] = true
		}
	}
	for _, rng := range unicode.Cf.R32 {
		for r := rune(rng.Lo); r <= rune(rng.Hi); r += rune(rng.Stride) {
			out[r] = true
		}
	}
	return out
}

// TestTheFormatRuneClassIsExactlyUnicodeCategoryCf: the predicate admits every
// FORMAT character and nothing else, over every code point.
func TestTheFormatRuneClassIsExactlyUnicodeCategoryCf(t *testing.T) {
	want := formatCodePoints()
	var missing, extra []rune
	for r := rune(0); r <= 0x10FFFF; r++ {
		switch got := IsFormatRune(r); {
		case want[r] && !got:
			missing = append(missing, r)
		case !want[r] && got:
			extra = append(extra, r)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%d FORMAT code point(s) are not recognised — the first %d %s. "+
			"An invisible character that is not recognised is one a renderer prints "+
			"raw, so two values that differ in bytes print alike",
			len(missing), min(8, len(missing)), sampleRunes(missing))
	}
	if len(extra) > 0 {
		t.Errorf("%d code point(s) are recognised that are not FORMAT characters — the first %d %s. "+
			"A character that is not invisible is one a reader can see, so refusing or "+
			"escaping it changes a value a reader could read",
			len(extra), min(8, len(extra)), sampleRunes(extra))
	}
}

// TestTheFormatRuneClassNamesTheInvisibleMembers: the members the class exists
// for are named one by one, because each is a character some future change
// would plausibly reach for. The exhaustive test above proves the SET matches;
// this says which members are the point.
func TestTheFormatRuneClassNamesTheInvisibleMembers(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    rune
	}{
		{"zero-width space", 0x200B},
		{"zero-width non-joiner", 0x200C},
		{"zero-width joiner", 0x200D},
		{"word joiner", 0x2060},
		{"byte-order mark", 0xFEFF},
		{"left-to-right mark", 0x200E},
		{"right-to-left override", 0x202E},
		{"left-to-right isolate", 0x2066},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !IsFormatRune(tc.r) {
				t.Errorf("%U is not recognised as a FORMAT character, so a renderer "+
					"prints it raw and two values that differ in bytes print alike", tc.r)
			}
		})
	}
}
