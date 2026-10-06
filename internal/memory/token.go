package memory

// The stored-value renderer: ONE allowlist for every place Ghost prints text a
// session or an operator chose, outside the «...» data delimiters.
//
// It lives here rather than in internal/assemble, where it was, because the
// dependency runs assemble -> memory and not the other way round: the audit's
// usefulness line renders a session id into a prompt, and a reader in internal/memory
// cannot call a function in the package that imports it. internal/assemble.Token
// now delegates to SafeToken rather than keeping its own copy, so there is ONE
// implementation of the rule — the same arrangement the stamp parsers already use
// (memory.ParseStamp, memory.StampLayouts, memory.ValidityState) and for the same
// reason: two implementations of one rule are two rules, and the copy nobody
// tests is the one that ships the bug.
//
// The rule is an ALLOWLIST, and that is the whole point. What it replaces is a
// denylist — neutralSessionID in usefulness.go, this package's usefulness reader —
// and a denylist can only list what somebody thought of: it let U+2028 and U+0085 through (both LINE TERMINATORS to
// JavaScript and to several newline-splitting readers), a bidi override that
// reverses how the rest of a line displays, and an invisible zero-width space that
// a reader cannot see in the output at all. Each of those is a way for stored text
// to end a line, reorder a line, or hide in one. An allowlist cannot have that
// problem, because anything not named is quoted rather than trusted.

import "strconv"

// SafeToken renders one stored value that a line prints OUTSIDE the «...» data
// delimiters, written bare only when every character is one a stored name
// plausibly uses, and as an ASCII-only Go quoted string otherwise.
//
// Quoting is not a punishment for the honest case — it never applies to it. The
// ids Ghost mints are 32 hex characters and a scope name is a word, all of which
// the bare set covers, so every real row prints exactly as it is stored and the
// escaping is invisible except where it is doing work. What the quoted form buys
// is that no character outside the bare set can start a line of its own, close the
// construct it sits in, open a data block, reorder what a reader sees, or hide
// from one.
//
// The empty string renders as `""`, not as nothing, so a field that printed an id
// always prints SOMETHING there: a reader cannot tell "the value was empty" from
// "the line was truncated before this field", and only one of those is true.
func SafeToken(s string) string {
	if s == "" {
		return `""`
	}
	for _, r := range s {
		if !IsSafeTokenRune(r) {
			return strconv.QuoteToASCII(s)
		}
	}
	return s
}

// IsSafeTokenRune reports whether r is in the set SafeToken writes bare.
//
// It is exported for the ONE reason Token is: internal/assemble has three
// renderers that consult the set a rune at a time rather than on a whole string,
// and a second copy of this set is a second rule. It is deliberately a single
// named predicate rather than a documented set that callers could rebuild, because
// rebuilding it is how a copy appears.
//
// Nothing here is added to the set to make some real id prettier. An id this repo
// mints is hex; an id a user supplies is stored verbatim and quoted rather than
// mangled silently.
func IsSafeTokenRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return r == '.' || r == '_' || r == '-' || r == ':' || r == '/' || r == '@' || r == '+'
}
