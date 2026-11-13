package assemble

import (
	"strconv"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/adversarial"
)

// TestPreviewLineCutsAtACarriageReturnAsWellAsANewline is the route-3 guarantee
// the docs claim, pinned at the one function that now provides it.
func TestPreviewLineCutsAtACarriageReturnAsWellAsANewline(t *testing.T) {
	for name, in := range map[string]string{
		"a lone carriage return":     "legitimate claim\roverwrite this",
		"a trailing carriage return": "legitimate claim\r",
		"a CRLF pair":                "legitimate claim\r\nsecond line",
		"a CR before a LF":           "one\rtwo\nthree",
		"a CR after the cap":         "1234567890\rforged",
		"only a CR":                  "\r",
		"only a CRLF":                "\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			// A cap no case can reach, so the assertion is about the line break
			// and not about truncation: the rune cap cannot introduce one.
			got := PreviewLine(in, 200)
			if strings.ContainsAny(got, "\r\n") {
				t.Errorf("PreviewLine(%q) = %q, which still carries a line break", in, got)
			}
			// And it is the FIRST line, not the last, and not the whole string: a lone
			// CR breaks a line exactly as much as a LF.
			want := in
			if i := strings.IndexAny(in, "\r\n"); i >= 0 {
				want = in[:i]
			}
			if got != want {
				t.Errorf("PreviewLine(%q) = %q, want the first line %q", in, got, want)
			}
		})
	}
}

// TestPreviewLineEscapesControlsAndFormatCharacters is #1689: PreviewLine must
// escape C0/C1 control characters (except TAB) and invisible FORMAT characters
// (category Cf) the same way Data, Label and TagsLabel do.
func TestPreviewLineEscapesControlsAndFormatCharacters(t *testing.T) {
	// Control characters that Data escapes (C0 except TAB, C1, DEL)
	controlCases := []struct {
		name string
		r    rune
		want string // strconv.QuoteRuneToASCII without the surrounding quotes
	}{
		{"ESC", 0x1b, `\x1b`},
		{"BEL", 0x07, `\a`},
		{"NUL", 0x00, `\x00`},
		{"DEL", 0x7f, `\x7f`},
		{"U+0080", 0x80, `\u0080`},
		{"CSI U+009B", 0x9b, `\u009b`},
		{"U+009F", 0x9f, `\u009f`},
	}
	for _, c := range controlCases {
		t.Run("control/"+c.name, func(t *testing.T) {
			in := "prefix" + string(c.r) + "suffix"
			got := PreviewLine(in, 200)
			// The control rune must be escaped
			if strings.ContainsRune(got, c.r) {
				t.Errorf("PreviewLine printed U+%04X raw: %q", c.r, got)
			}
			// The escape must be the strconv ASCII form
			if !strings.Contains(got, c.want) {
				t.Errorf("PreviewLine(%q) = %q, want escape %q present", in, got, c.want)
			}
		})
	}
	// TAB is the one control character Data keeps raw
	t.Run("control/TAB", func(t *testing.T) {
		in := "prefix\tsuffix"
		got := PreviewLine(in, 200)
		if got != in {
			t.Errorf("PreviewLine(TAB) = %q, want raw tab preserved", got)
		}
	})

	// Format characters (category Cf) - the ZeroWidth and bidi controls
	for _, r := range adversarial.FormatRunes() {
		r := r // capture loop variable
		t.Run("format/"+strconv.FormatInt(int64(r), 16), func(t *testing.T) {
			in := "gh" + string(r) + "ost"
			got := PreviewLine(in, 200)
			if strings.ContainsRune(got, r) {
				t.Errorf("PreviewLine printed format U+%04X raw: %q", r, got)
			}
			// The escape must be the strconv ASCII form
			want := strconv.QuoteRuneToASCII(r)
			want = want[1 : len(want)-1] // strip quotes
			if !strings.Contains(got, want) {
				t.Errorf("PreviewLine(%q) = %q, want escape %q present", in, got, want)
			}
			// Two values differing only in a format character must not render identically
			plain := "ghost"
			if stripFormatCharacters(PreviewLine(plain, 200)) == stripFormatCharacters(got) {
				t.Errorf("PreviewLine rendered %q and %q identically to a reader", plain, in)
			}
		})
	}
}

// TestPreviewLineCapsAndPassesOrdinaryText pins the rune cap, an over-cap cut
// through invalid UTF-8 and through an escape, and a negative cap.
func TestPreviewLineCapsAndPassesOrdinaryText(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"under the cap", "a claim", 8, "a claim"},
		{"first of two lines", "first\nsecond", 8, "first"},
		{"over the cap", "日本語のテキストです", 8, "日本語のテキスト…"},
		{"invalid UTF-8 over the cap", "\xff\xff\xff", 2, "��…"},
		{"an escape that would overrun", "ab\u202ecd", 4, "ab…"},
		{"a negative cap", "abc", -1, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := PreviewLine(tc.in, tc.max); got != tc.want {
				t.Errorf("PreviewLine(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
		})
	}
}
