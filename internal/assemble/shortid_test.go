package assemble

import (
	"testing"
	"unicode/utf8"
)

// TestShortIDMeasuresEightCharactersAndRendersThroughTheSafeToken is #810: a
// byte-sliced id on a CJK id returned invalid UTF-8 inside a trace note.
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
