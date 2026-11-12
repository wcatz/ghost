package adversarial

import (
	"strings"
	"unicode"
)

// FormatControls returns text carrying a control that must never be printed raw
// on a line a human or an agent reads: bidi overrides and isolates, line and
func FormatControls() []Payload {
	return []Payload{
		{Name: "bidi-lre", Text: "before\u202abinside\u202cafter"},
		{Name: "bidi-rle", Text: "before\u202bbinside\u202cafter"},
		{Name: "bidi-pdf", Text: "before\u202cinside after"},
		{Name: "bidi-lro", Text: "before\u202dinside\u202cafter"},
		{Name: "bidi-rlo", Text: "before\u202einside\u202cafter"},
		{Name: "bidi-lri", Text: "before\u2066inside\u2069after"},
		{Name: "bidi-rli", Text: "before\u2067inside\u2069after"},
		{Name: "bidi-fsi", Text: "before\u2068inside\u2069after"},
		{Name: "bidi-pdi", Text: "before\u2069inside after"},
		{Name: "bidi-lrm", Text: "before\u200einside after"},
		{Name: "bidi-rlm", Text: "before\u200finside after"},
		{Name: "next-line", Text: "before\u0085after"},
		{Name: "escape", Text: "before\x1b[2Jafter"},
		{Name: "vertical-tab", Text: "before\vafter"},
		{Name: "form-feed", Text: "before\fafter"},
	}
}

// LineSeparators returns characters that end a line without being a control
// character. Renderers must fold or escape them, and a reader that only asks
func LineSeparators() []Payload {
	return []Payload{
		{Name: "line-separator", Text: "before\u2028after"},
		{Name: "paragraph-separator", Text: "before\u2029after"},
	}
}

// line: they hide a spelling from a reader, so a rendered row that prints one
func ZeroWidth() []Payload {
	return []Payload{
		{Name: "zero-width-space", Text: "gh\u200bost"},
		{Name: "zero-width-non-joiner", Text: "gh\u200cost"},
		{Name: "zero-width-joiner", Text: "gh\u200dost"},
		{Name: "word-joiner", Text: "gh\u2060ost"},
		{Name: "byte-order-mark", Text: "gh\ufeffost"},
	}
}

// HasZeroWidth reports whether s carries one of the zero-width characters in
// ZeroWidth. A boundary test uses it to tell the known gap from a regression:
func HasZeroWidth(s string) bool {
	for _, r := range s {
		switch r {
		case 0x200b, 0x200c, 0x200d, 0x2060, 0xfeff:
			return true
		}
	}
	return false
}

// FormatRunes returns every Unicode FORMAT character (category Cf) — the class
// a renderer must make visible and a write boundary must refuse.
func FormatRunes() []rune {
	var out []rune
	for _, rng := range unicode.Cf.R16 {
		for r := rune(rng.Lo); r <= rune(rng.Hi); r += rune(rng.Stride) {
			out = append(out, r)
		}
	}
	for _, rng := range unicode.Cf.R32 {
		for r := rune(rng.Lo); r <= rune(rng.Hi); r += rune(rng.Stride) {
			out = append(out, r)
		}
	}
	return out
}

// AllControls is FormatControls, LineSeparators and ZeroWidth, for the render
// boundaries whose job is to keep every stored character legible and on one
func AllControls() []Payload {
	out := FormatControls()
	out = append(out, LineSeparators()...)
	return append(out, ZeroWidth()...)
}

// TokenShaped returns values the token guard recognises as a token. They are
// inert at a boundary whose whole job is to render stored text: the value may be
func TokenShaped() []Payload {
	return []Payload{
		{Name: "github-token", Text: "ghp_0123456789abcdefghijklmnopqrstuvwxyzAB"},
		{Name: "aws-access-key", Text: "AKIAIOSFODNN7EXAMPLE"},
		{Name: "age-key", Text: "AGE-SECRET-KEY-1QP7X2ZK8M4N6RT9WVYC3JFD5A0BG6E7H2K"},
		{Name: "bearer", Text: "Bearer ghp_0123456789abcdefghijklmnopqrstuvwxyzAB"},
	}
}

// Oversized returns a field far past every write-time bound the store and the
// tools apply, so a boundary that is supposed to bound what it prints, or to
func Oversized() []Payload {
	long := strings.Repeat("a", 4096) + "token"
	return []Payload{
		{Name: "oversized-project", Text: long},
		{Name: "oversized-tag", Text: long},
		{Name: "oversized-id", Text: long},
		{Name: "oversized-content", Text: strings.Repeat("memory text ", 600)},
	}
}

// MarkerPathShapes names the entries that are not a regular marker file. The
// marker path lives in the data directory, which every local process can write,
func MarkerPathShapes() []string {
	return []string{"fifo", "symlink-to-fifo", "symlink-to-marker", "directory"}
}

// MalformedStamps returns validity-stamp values that no accepted layout reads.
// The save tool refuses each of them rather than storing a claim nobody can
func MalformedStamps() []Payload {
	return []Payload{
		{Name: "not-a-date", Text: "not-a-date"},
		{Name: "reversed-date", Text: "2026-31-12"},
		{Name: "hour-out-of-range", Text: "2026-10-01T25:00:00Z"},
		{Name: "missing-zone", Text: "2026-10-01T09:00:00"},
	}
}
