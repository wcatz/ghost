package assemble

import (
	"strings"
	"testing"
)

// TestPreviewLineCutsAtACarriageReturnAsWellAsANewline is the route-3 guarantee
// the docs claim, pinned at the one function that now provides it.
//
// A preview is rendered raw — that is what makes it a preview rather than a
// quoted field — so it is the one place stored text reaches a reader with no
// «...» around it. What makes that safe is the truncation, and the truncation
// used to cut at '\n' only, in three separate copies. A memory whose content was
// `legitimate claim\roverwrite this` therefore previewed as `overwrite this`,
// because a terminal reads CR as "return to column 0 and overwrite", and several
// renderers split on CR as readily as on LF.
//
// The test lives here, once, because the copies are gone: `assemble.PreviewLine`
// is the only implementation, and its three former homes — the MCP resolve and
// link-withdraw reports, the CLI's stored-row display, and the import report's
// content preview — all call it. A test per copy is what let the copies drift.
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
			// And it is the FIRST line, not the last, and not the whole string: a
			// lone CR breaks a line exactly as much as a LF, and a CRLF must not
			// be cut so that the empty string after the CR is what comes back.
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

// TestPreviewLineStillTruncatesAndStillPassesOrdinaryText guards the two
// properties the rune cap and the ordinary case depend on, so the CR fix cannot
// have been the price of either. These are the byte shapes the three former
// copies produced for ordinary content, and they must not have moved.
func TestPreviewLineStillTruncatesAndStillPassesOrdinaryText(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"empty":                   {"", ""},
		"one line, under the cap": {"a claim", "a claim"},
		"one line, at the cap":    {"12345678", "12345678"},
		"one line, over the cap":  {"123456789", "12345678…"},
		"first of two lines":      {"first\nsecond", "first"},
		"a unicode line, over":    {"日本語のテキストです", "日本語のテキスト…"},
		"a unicode line, under":   {"日本語", "日本語"},
		"a blank line":            {"\ncontent after", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := PreviewLine(tc.in, 8); got != tc.want {
				t.Errorf("PreviewLine(%q, 8) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
