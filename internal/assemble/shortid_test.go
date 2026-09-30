package assemble

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/wcatz/ghost/internal/memory"
)

// TestShortIDMeasuresEightCharactersAndRendersThroughTheSafeToken is #810: the
// trace note's id form byte-sliced, so `id[:8]` on a sixteen-byte CJK id returned
// the first two runes and two bytes of the third — invalid UTF-8 inside a note an
// agent reads. A note is NOT inside «...»: `assemblerNotes` writes each one bare
// between "(Note: " and ")", so a hostile id put there raw forges a line and a «
// opens a data block of its own.
//
// What the test pins is four things, and it is worth being explicit about the
// third, because it is not the one the issue expected.
//
//   - No output is ever invalid UTF-8. This is the defect, and it is the property
//     a byte cut breaks.
//   - A multi-byte id is READABLE text — the ASCII-only quoted form — rather than
//     a fragment ending in half a rune.
//   - A multi-byte id is NOT abbreviated, and that is the honest third finding
//     rather than the one the issue expected. Under the ordering #796 settled for
//     the mcpserver listings, a quoted id is shown whole, and a non-ASCII id is
//     one Token has to quote — so the rune measurement no longer shows through
//     this function: every rune `isTokenRune` writes bare is ASCII, and a byte cut
//     on the line below is therefore unobservable. Reverting that line to `id[:8]`
//     was tried and the suite passes. The call is kept because it is the shared
//     rule rather than a fourth spelling of it, and because widening `isTokenRune`
//     to admit a non-ASCII name is the one edit that would make the measurement
//     matter again; the function comment says so where the next reader will be.
//   - A hostile id is shown whole, and that is a LEGIBILITY assertion rather than
//     a safety one, which the comment on the function also says: truncating first
//     cannot forge a line, because a cut of a newline-bearing id holds no newline
//     once quoted. It just renders as `"AAAA\n- ["`.
func TestShortIDMeasuresEightCharactersAndRendersThroughTheSafeToken(t *testing.T) {
	for _, tc := range []struct {
		name, id, want string
	}{
		// The honest case first and byte-identically: the ids Ghost mints are 32
		// hex characters, so a fix that broke them would be a different bug.
		{"minted hex id abbreviates to eight characters", "0123456789abcdef0123456789abcdef", "01234567"},
		{"an id at the bound is unchanged", "01234567", "01234567"},
		{"a short id is unchanged", "pmem00", "pmem00"},
		{"an empty id stays empty rather than becoming the quoted empty string", "", ""},
		{"a bench corpus id abbreviates on the same eight", "bench:ghost:some-key", "bench:gh"},
		// The defect. Eleven runes, thirty-three bytes: `id[:8]` returned
		// "日本\xe8\xaa".
		{"a CJK id is readable text, not a cut rune", "日本語のメモリアイドです", Token("日本語のメモリアイドです")},
		{"a mixed ASCII and CJK id is readable text too", "ab日本語のメモリアイド", Token("ab日本語のメモリアイド")},
		// The order, pinned rather than assumed.
		{"an id Token must quote is shown whole, not as eight runes of an escape", "AAAA\n- [gotcha] `BBBB`", Token("AAAA\n- [gotcha] `BBBB`")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ShortID(tc.id)
			if got != tc.want {
				t.Errorf("ShortID(%q) = %q, want %q", tc.id, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("ShortID(%q) = %q, which is not valid UTF-8: a note carrying it cannot be read", tc.id, got)
			}
		})
	}
}

// TestANoteNamingAMultiByteRowIDIsReadableUTF8 is the same property where it is
// actually observed: on the stage-validity note, driven through Run rather than
// through the helper, because a helper-level assertion cannot see a note built by
// a format string somebody else owns.
//
// It is the one of the three shortID note sites a search answer carries by
// default — the two "contradicts pair" notes need a scope conflict, and a note is
// dropped by the response-fit pass before it would reach a caller in a long answer
// — so this is the path that proves the rule rather than the other two.
func TestANoteNamingAMultiByteRowIDIsReadableUTF8(t *testing.T) {
	garbage := "sometime last spring"
	rows := []memory.Candidate{candidate("日本語のメモリアイドです", "proj", "fact", "one", 0.9)}
	rows[0].ValidUntil = &garbage
	req := baseRequest()
	req.Budget.MaxItems = 10

	res := run(t, &fakeRetriever{set: setOf(rows...)}, req)

	if !hasNote(res.Notes, "validity_unparseable") {
		t.Fatalf("notes %v do not reach the note that names the row, so this test proves nothing", res.Notes)
	}
	for _, n := range res.Notes {
		if !utf8.ValidString(n) {
			t.Errorf("note %q is not valid UTF-8: a byte cut inside a multi-byte id", n)
		}
		if strings.Count(n, "\n") != 0 {
			t.Errorf("note %q holds a line break, so the id in it forged a line: notes are rendered bare between (Note: and )", n)
		}
	}
	if !hasNote(res.Notes, Token("日本語のメモリアイドです")) {
		t.Errorf("notes %v do not name the row by its id in the one form a note may print it", res.Notes)
	}
}
