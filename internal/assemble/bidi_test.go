package assemble

import (
	"strconv"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/adversarial"
)

// bidiRunes is the set of bidirectional controls Data and Label must never print
// raw. They are category Cf, so unicode.IsControl does not cover them.
var bidiRunes = []rune{
	0x200E, 0x200F,
	0x202A, 0x202B, 0x202C, 0x202D, 0x202E,
	0x2066, 0x2067, 0x2068, 0x2069,
}

func hasBidiRune(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool {
		for _, b := range bidiRunes {
			if r == b {
				return true
			}
		}
		return false
	}) >= 0
}

// TestBidiControlsAreEscapedInDataAndLabel feeds the adversarial bidi payloads
// through both renderers and asserts no raw bidi rune survives and the escape
// form does.
func TestBidiControlsAreEscapedInDataAndLabel(t *testing.T) {
	for _, p := range adversarial.UnicodeTrick() {
		if !hasBidiRune(p.Text) {
			continue
		}
		for _, tc := range []struct {
			name string
			fn   func(string) string
		}{
			{"Data", Data},
			{"Label", Label},
		} {
			out := tc.fn(p.Text)
			if hasBidiRune(out) {
				t.Errorf("%s(%q) left a raw bidi rune: %q", tc.name, p.Name, out)
			}
			if !strings.Contains(out, `\u202e`) && !strings.Contains(out, `\u202c`) {
				t.Errorf("%s(%q) missing bidi escape form: %q", tc.name, p.Name, out)
			}
		}
	}
}

// TestDataAndLabelEscapeEveryBidiControl checks each rune of the set individually.
func TestDataAndLabelEscapeEveryBidiControl(t *testing.T) {
	for _, r := range bidiRunes {
		in := "a" + string(r) + "b"
		esc := strconv.QuoteRuneToASCII(r)
		esc = esc[1 : len(esc)-1]
		for _, tc := range []struct {
			name string
			fn   func(string) string
		}{
			{"Data", Data},
			{"Label", Label},
		} {
			out := tc.fn(in)
			if hasBidiRune(out) {
				t.Errorf("%s(U+%04X) left a raw bidi rune: %q", tc.name, r, out)
			}
			if !strings.Contains(out, esc) {
				t.Errorf("%s(U+%04X) = %q, want escape %q", tc.name, r, out, esc)
			}
		}
	}
}

// TestDataLeavesOrdinaryUnicodeByteIdentical asserts common non-ASCII text is
// untouched by Data.
func TestDataLeavesOrdinaryUnicodeByteIdentical(t *testing.T) {
	s := "café\tnaïve 日本語 Привет"
	if got, want := Data(s), "«"+s+"»"; got != want {
		t.Errorf("Data(%q) = %q, want %q", s, got, want)
	}
}
