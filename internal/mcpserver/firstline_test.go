package mcpserver

import (
	"strings"
	"testing"
)

// TestFirstLineCutsAtACarriageReturnAsWellAsANewline is the route-3 guarantee the
// docs claim, pinned at the function that provides it.
//
// A preview is rendered raw into a tool answer, so it is the one place stored
// text reaches an agent WITHOUT the «...» delimiters — deliberately, because
// labelling a 70-character preview as data would misrepresent what it is. What
// makes that safe is the truncation, and the truncation used to cut at '\n' only.
// A memory whose content was `legitimate claim\roverwrite this` therefore printed
// a preview holding a carriage return, which a terminal reads as "return to
// column 0 and overwrite" and which several renderers split on as readily as on
// LF. The docs sentence the reviewer caught said "cut at the first newline", and
// the honest fix was the function rather than a weaker sentence.
func TestFirstLineCutsAtACarriageReturnAsWellAsANewline(t *testing.T) {
	for name, in := range map[string]string{
		"a lone carriage return":     "legitimate claim\roverwrite this",
		"a trailing carriage return": "legitimate claim\r",
		"a CRLF pair":                "legitimate claim\r\nsecond line",
		"a CR before a LF":           "one\rtwo\nthree",
		"a CR after the cap":         "1234567890\r1234567890",
		"only a CR":                  "\r",
		"only a CRLF":                "\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			// A generous cap, so the assertion is about the line break and not
			// about truncation: the rune cap cannot introduce one.
			got := firstLine(in, 200)
			if strings.ContainsAny(got, "\r\n") {
				t.Errorf("firstLine(%q) = %q, which still carries a line break", in, got)
			}
			// And it is the FIRST line, not the last, and not the whole string:
			// a lone CR is a break exactly as much as a LF, and a CRLF must not
			// be cut so that the empty string after the CR comes back.
			want := in
			if i := strings.IndexAny(in, "\r\n"); i >= 0 {
				want = in[:i]
			}
			if got != want {
				t.Errorf("firstLine(%q) = %q, want the first line %q", in, got, want)
			}
		})
	}
}

// TestFirstLineStillTruncatesAndStillPassesOrdinaryText guards the two properties
// the rune cap and the ordinary case depend on, so the CR fix cannot have been
// the price of either.
func TestFirstLineStillTruncatesAndStillPassesOrdinaryText(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"empty":                   {"", ""},
		"one line, under the cap": {"a claim", "a claim"},
		"one line, at the cap":    {"12345678", "12345678"},
		"one line, over the cap":  {"123456789", "12345678…"},
		"first of two lines":      {"first\nsecond", "first"},
		"a unicode first line":    {"日本語のテキストです", "日本語のテキスト…"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := firstLine(tc.in, 8); got != tc.want {
				t.Errorf("firstLine(%q, 8) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
