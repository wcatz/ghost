package adversarial

import "strings"

// This file is the shared hostile-input set for the boundaries where stored or
// caller-supplied text leaves the store (#1652). The parse-surface corpus above
// exists so a relaxation of the parse invariant fails every suite at once; this
// set does the same job for the OUTWARD boundaries: rendering in assemble, MCP
// tool results, log lines, the import path and the lifecycle marker files.
//
// The classes are the ones that have recurred as separate defects at a second
// boundary after being fixed at the first: bidi and other format controls,
// token-shaped strings, oversized fields, malformed stamps, and non-regular
// files at marker paths. A boundary test picks the constructor for the class it
// is about rather than spelling its own string, so a class that is extended
// here is exercised everywhere the constructor is used.

// FormatControls returns text carrying a control that must never be printed raw
// on a line a human or an agent reads: bidi overrides and isolates, line and
// paragraph separators, the C1 next-line control, and the C0 controls a terminal
// acts on. They are data, so a renderer may escape them but may not drop or
// reorder them silently.
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
// unicode.IsControl will miss both.
func LineSeparators() []Payload {
	return []Payload{
		{Name: "line-separator", Text: "before\u2028after"},
		{Name: "paragraph-separator", Text: "before\u2029after"},
	}
}

// AllControls is FormatControls plus LineSeparators, for the render boundaries
// whose job is to keep every character on one physical line.
func AllControls() []Payload {
	out := FormatControls()
	return append(out, LineSeparators()...)
}

// TokenShaped returns values the token guard recognises as a credential. They
// are inert at a boundary whose whole job is to render stored text: the value
// may be shown through the guard's own placeholder, but never replayed raw.
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
// withhold an oversized value instead of quoting it, is exercised.
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
// so each of these is reachable by planting an entry where a marker belongs.
func MarkerPathShapes() []string {
	return []string{"fifo", "symlink-to-fifo", "symlink-to-marker", "directory"}
}

// MalformedStamps returns validity-stamp values that no accepted layout reads.
// The save tool refuses each of them rather than storing a claim nobody can
// interpret, and the import path is expected to apply the same rules.
func MalformedStamps() []Payload {
	return []Payload{
		{Name: "not-a-date", Text: "not-a-date"},
		{Name: "reversed-date", Text: "2026-31-12"},
		{Name: "hour-out-of-range", Text: "2026-10-01T25:00:00Z"},
		{Name: "missing-zone", Text: "2026-10-01T09:00:00"},
	}
}
