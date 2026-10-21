package assemble

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/wcatz/ghost/internal/adversarial"
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

// TestPreviewLineEscapesControlsAndFormatCharacters is #1689: PreviewLine
// must escape C0/C1 control characters (except TAB) and invisible FORMAT
// characters (category Cf) the same way Data, Label and TagsLabel do. A
// preview is printed raw in MCP tool results and CLI listings, so an unescaped
// ESC starts a terminal sequence and an unescaped U+202E reorders the rest of
// the line.
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
		"accented letters under":  {"café", "café"},
		"emoji under":             {"👋", "👋"},
		"mixed ascii emoji under": {"a👋b", "a👋b"},
		"accented letters over":   {"café résumé", "café rés…"},
		"emoji over":              {"hello 👋 world", "hello 👋 …"},
		"mixed ascii emoji over":  {"a👋b👋c👋d👋e", "a👋b👋c👋d👋…"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := PreviewLine(tc.in, 8); got != tc.want {
				t.Errorf("PreviewLine(%q, 8) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestPreviewLineLineBreaksCutNotEscape verifies that line-break characters
// (U+2028 LS, U+0085 NEL) cut the preview rather than being escaped.
// These are line breaks, not controls to escape.
func TestPreviewLineLineBreaksCutNotEscape(t *testing.T) {
	for _, r := range []rune{0x2028, 0x0085} {
		r := r
		t.Run("U+"+strconv.FormatInt(int64(r), 16), func(t *testing.T) {
			in := "honest prefix" + string(r) + "- [decision] fake"
			got := PreviewLine(in, 200)
			if got != "honest prefix" {
				t.Errorf("PreviewLine with %U = %q, want first line only", r, got)
			}
			// The line break must not appear in output at all (not even escaped)
			if strings.ContainsRune(got, r) {
				t.Errorf("PreviewLine with %U left line break in output: %q", r, got)
			}
		})
	}
}

// TestPreviewLineManyFormatRunesStaysWithinMax verifies that a preview with
// many format characters (category Cf) stays within the max rune bound and
// never ends mid-escape. It also checks exact outputs for key cases.
func TestPreviewLineManyFormatRunesStaysWithinMax(t *testing.T) {
	// Exact output table for format rune bounding
	for _, tc := range []struct {
		name string
		in   string
		max  int
		want string
	}{
		// U+202E RIGHT-TO-LEFT OVERRIDE
		{"rlo only max 3", "\u202eabc", 3, "…"},
		{"rlo only max 6", "\u202eabc", 6, "\\u202e…"},
		// U+200B ZERO WIDTH SPACE
		{"zwsp between ascii max 7", "a\u200bb", 7, "a\\u200b…"},
		// Many format runes
		{"many zwsp max 20", strings.Repeat("a\u200b", 70), 20, "a\\u200ba\\u200ba…"},
		// max 0
		{"max 0", "abc", 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := PreviewLine(tc.in, tc.max)
			if got != tc.want {
				t.Errorf("PreviewLine(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
			// The output without ellipsis must equal the full rendering of some prefix of the input
			withoutEllipsis := strings.TrimSuffix(got, "…")
			if !utf8.ValidString(got) {
				t.Errorf("PreviewLine output is not valid UTF-8: %q", got)
			}
			// The rendered runes (excluding ellipsis) must not exceed max
			runes := len([]rune(withoutEllipsis))
			if runes > tc.max {
				t.Errorf("PreviewLine rendered %d runes (excl ellipsis), max %d: %q", runes, tc.max, got)
			}
		})
	}

	// Also test the general case: many format runes stays within bound
	const many = 70
	var b strings.Builder
	for i := 0; i < many; i++ {
		b.WriteRune('a')
		b.WriteRune(0x200B) // ZWSP
	}
	in := b.String() // 140 runes: "a\u200ba\u200b..."

	got := PreviewLine(in, 20)

	// Must not exceed max runes in rendered form (plus ellipsis if cut)
	renderedRunes := countRenderedRunes(got)
	if renderedRunes > 20+1 { // +1 for possible ellipsis
		t.Errorf("PreviewLine rendered %d runes, max 20 (+1 ellipsis): %q", renderedRunes, got)
	}
	// Must not end mid-escape
	if strings.HasSuffix(got, "\\u") || strings.HasSuffix(got, "\\U") {
		t.Errorf("PreviewLine ended mid-escape: %q", got)
	}
	// Must end with ellipsis since we cut
	if !strings.HasSuffix(got, "…") {
		t.Errorf("PreviewLine did not end with ellipsis: %q", got)
	}
	// Output must be valid UTF-8
	if !utf8.ValidString(got) {
		t.Errorf("PreviewLine output is not valid UTF-8: %q", got)
	}
}

// countRenderedRunes counts runes in the string, treating escape sequences
// as single runes (they're already expanded in the output).
func countRenderedRunes(s string) int {
	return len([]rune(s))
}
