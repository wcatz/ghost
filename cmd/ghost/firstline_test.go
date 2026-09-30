package main

import (
	"strings"
	"testing"
)

// TestFirstLineCutsAtACarriageReturnAsWellAsANewline is the CLI half of the same
// guarantee internal/mcpserver's firstLine provides, and it exists because the
// two copies are meant to stay parallel — this one's own doc comment says it
// mirrors the MCP one — so a fix to one that is not made to the other leaves the
// pair claiming a relationship they no longer have.
//
// Here the terminal is the surface, which makes a lone carriage return worse
// than cosmetic rather than merely unstated: `legitimate claim\roverwrite this`
// renders as `overwrite this`, because a terminal returns to column 0 on CR and
// writes over what is there.
func TestFirstLineCutsAtACarriageReturnAsWellAsANewline(t *testing.T) {
	for name, in := range map[string]string{
		"a lone carriage return":     "legitimate claim\roverwrite this",
		"a trailing carriage return": "legitimate claim\r",
		"a CRLF pair":                "legitimate claim\r\nsecond line",
		"a CR before a LF":           "one\rtwo\nthree",
		"only a CR":                  "\r",
		"only a CRLF":                "\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			got := firstLine(in, 200)
			if strings.ContainsAny(got, "\r\n") {
				t.Errorf("firstLine(%q) = %q, which still carries a line break", in, got)
			}
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

// TestFirstLineStillTruncatesAndStillPassesOrdinaryText is the same guard the
// MCP copy carries, for the two properties the rune cap and the ordinary case
// depend on.
func TestFirstLineStillTruncatesAndStillPassesOrdinaryText(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"empty":                   {"", ""},
		"one line, under the cap": {"a claim", "a claim"},
		"one line, at the cap":    {"12345678", "12345678"},
		"one line, over the cap":  {"123456789", "12345678…"},
		"first of two lines":      {"first\nsecond", "first"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := firstLine(tc.in, 8); got != tc.want {
				t.Errorf("firstLine(%q, 8) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
