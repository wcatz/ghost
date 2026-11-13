package memory

// The bare-token allowlist, pinned EXACTLY. SafeToken writes a value bare when
// every rune is in the set and quotes the whole string otherwise, so a rune
// wrongly admitted (a control character, U+2028/U+2029, a bidi control, a
// zero-width or non-breaking space) reaches a line an agent reads as Ghost's
// own. A table of hostile examples passes whenever the set is widened by a
// character nobody listed, so the membership is enumerated over every code point.

import (
	"fmt"
	"strings"
	"testing"
)

// TestTheBareTokenAllowlistIsExactlyTheDocumentedSet: membership is exactly ASCII
// letters, ASCII digits and the seven separators Ghost's own id and scope shapes
// use, and each separator renders bare.
func TestTheBareTokenAllowlistIsExactlyTheDocumentedSet(t *testing.T) {
	const allowed = "._-:/@+"
	inSet := func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune(allowed, r)
	}

	var extra, missing []string
	for r := rune(0); r <= 0x10FFFF; r++ {
		switch want, got := inSet(r), IsSafeTokenRune(r); {
		case want && !got:
			missing = append(missing, fmt.Sprintf("%U", r))
		case !want && got:
			extra = append(extra, fmt.Sprintf("%U", r))
		}
	}
	if len(extra) > 0 {
		t.Errorf("%d code point(s) are in the bare set that must not be, first: %v", len(extra), extra[:min(8, len(extra))])
	}
	if len(missing) > 0 {
		t.Errorf("%d documented code point(s) are missing from the bare set, first: %v", len(missing), missing[:min(8, len(missing))])
	}
	for _, r := range allowed {
		if v := "id" + string(r) + "v"; SafeToken(v) != v {
			t.Errorf("SafeToken(%q) = %q, want it written bare", v, SafeToken(v))
		}
	}
}

// sampleRunes renders the first few offenders for a failure message, bounded
// because a widened set can admit a hundred thousand code points.
func sampleRunes(rs []rune) string {
	out := ""
	for i, r := range rs {
		if i == 8 {
			return out + " (and more)"
		}
		if i > 0 {
			out += ", "
		}
		out += fmt.Sprintf("%c %U", r, r)
	}
	return out
}

// TestTheAstralCeilingIsTheRealOne: SafeToken's worst-case expansion is ten
// characters for a rune above U+FFFF and six for a BMP rune, so a caller that
// bounds a stored value by runes can size its output by that ceiling.
func TestTheAstralCeilingIsTheRealOne(t *testing.T) {
	for _, tc := range []struct {
		name       string
		r          string
		wantEscape int
	}{
		{"a BMP escaping rune", "\u202e", 6},
		{"an astral escaping rune", "\U000e0001", 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := SafeToken(tc.r)
			if len(got) != tc.wantEscape+2 {
				t.Errorf("SafeToken(%q) = %q, %d bytes; expected %d characters escaped plus the two quotes",
					tc.r, got, len(got), tc.wantEscape)
			}
		})
	}
}

// TestSafeTokenWritesARealNameBareAndQuotesEverythingElse: every real id stays
// byte-identical, an empty field still prints something, and anything else is
// quoted onto one line.
func TestSafeTokenWritesARealNameBareAndQuotesEverythingElse(t *testing.T) {
	for _, bare := range []string{"a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4", "ses_2", "0", "a.b-c_d:e/f@g+h"} {
		if got := SafeToken(bare); got != bare {
			t.Errorf("SafeToken(%q) = %q, want it written bare", bare, got)
		}
	}
	if got := SafeToken(""); got != `""` {
		t.Errorf("SafeToken(\"\") = %q, want %q", got, `""`)
	}
	for _, in := range []string{"my session", "a\nb", "a«b", "メモ", `{"a":1}`, "a b", "a\tb"} {
		got := SafeToken(in)
		if got == in || strings.ContainsAny(got, "\n\r") {
			t.Errorf("SafeToken(%q) = %q, want one quoted line", in, got)
		}
	}
}
