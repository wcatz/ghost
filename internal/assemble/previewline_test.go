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

// TestPreviewLineManyFormatRunesStaysWithinMax pins the exact output of the
// rendered bound: an escape counts as its full rune length, a rune that does
// not fit is not split, and the ellipsis follows only a cut. The table also
// covers a literal U+FFFD (one rune) and an invalid byte (rendered U+FFFD).
func TestPreviewLineManyFormatRunesStaysWithinMax(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		max  int
		want string
	}{
		// U+202E RIGHT-TO-LEFT OVERRIDE renders as six runes, so it does not
		// fit in three and must not be split.
		{"rlo only max 3", "\u202eabc", 3, "…"},
		{"rlo only max 6", "\u202eabc", 6, "\\u202e…"},
		// U+200B ZERO WIDTH SPACE renders as six runes.
		{"zwsp between ascii max 7", "a\u200bb", 7, "a\\u200b…"},
		{"many zwsp max 20", strings.Repeat("a\u200b", 70), 20, "a\\u200ba\\u200ba…"},
		// A zero width still cuts, so the ellipsis is printed for non-empty
		// text exactly as it was.
		{"max 0", "abc", 0, "…"},
		{"empty max 0", "", 0, ""},
		{"negative max", "", -1, ""},
		// A literal U+FFFD is one rune, not three invalid bytes.
		{"literal replacement characters", "\uFFFD\uFFFD\uFFFDx", 3, "\uFFFD\uFFFD\uFFFD…"},
		// An invalid byte renders as U+FFFD and counts one rune.
		{"invalid byte", "a\xffb", 200, "a\uFFFDb"},
		{"invalid byte at bound", "a\xffb", 2, "a\uFFFD…"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := PreviewLine(tc.in, tc.max)
			if got != tc.want {
				t.Errorf("PreviewLine(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("PreviewLine(%q, %d) is not valid UTF-8: %q", tc.in, tc.max, got)
			}
			withoutEllipsis := strings.TrimSuffix(got, "…")
			if !fullRenderings(tc.in)[withoutEllipsis] {
				t.Errorf("PreviewLine(%q, %d) = %q: %q is not the full rendering of any prefix",
					tc.in, tc.max, got, withoutEllipsis)
			}
			if tc.max >= 0 && len([]rune(withoutEllipsis)) > tc.max {
				t.Errorf("PreviewLine(%q, %d) rendered %d runes before the ellipsis",
					tc.in, tc.max, len([]rune(withoutEllipsis)))
			}
		})
	}
}

// fullRenderings returns the full rendered form of every prefix of s, the empty
// prefix included. PreviewLine's output without its ellipsis must be one of
// these: anything else means it split an escape or dropped text.
func fullRenderings(s string) map[string]bool {
	out := map[string]bool{"": true}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && n == 1:
			b.WriteString("\uFFFD")
		case isEscapedControl(r):
			q := strconv.QuoteRuneToASCII(r)
			b.WriteString(q[1 : len(q)-1])
		default:
			b.WriteString(string(r))
		}
		i += n
		out[b.String()] = true
	}
	return out
}
