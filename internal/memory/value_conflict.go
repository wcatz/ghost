package memory

import (
	"strings"
	"unicode"
)

// statesDifferentValue reports whether two similar texts state DIFFERENT values
// of one fact, which makes the newer one a replacement rather than a second
// wording, and so not something a save may fold.
//
// mergeScore measures how much vocabulary two texts share, and a replacement
// shares almost all of it: "go.mod pins github.com/jackc/pgx/v4 at v4.18.1" and
// "go.mod now pins github.com/jackc/pgx/v5 at v5.6.0" differ only in the values.
// Folding such a pair linked the replacement to the claim it replaces as a
// 'duplicate' and strengthened the stale claim (importance and access count), so
// the one write that recorded a change in truth made the retired claim rank
// higher, and the near-duplicate demotion then penalised whichever of the two
// ranked lower on every read surface until a supersede pass ran.
//
// The rule is lexical and deterministic, and it is asked of value-like tokens
// only (valueTokens): a version, a number, a path, a dotted, slashed, underscored
// or key=value identifier, a camelCase or ALL-CAPS name. A fold is refused when
// EACH side holds a value token the other side lacks — one value swapped for
// another. A text that only ADDS a value to the other ("TTL is 300 seconds" /
// "TTL is 300 seconds, set in config.yaml") is still a restatement, and so is a
// pair whose values agree however the prose around them is reworded.
//
// It is the safe direction to be wrong in, the same trade foldOnlyEquivalent
// makes: a false refusal leaves two rows a supersede or reflect pass can still
// relate, while a false fold strengthens a stale claim and links its replacement
// to it as a copy.
func statesDifferentValue(a, b string) bool {
	va, vb := valueTokens(a), valueTokens(b)
	return lacksValue(va, vb) && lacksValue(vb, va)
}

// lacksValue reports whether `from` holds a value token `other` does not have.
// A number with a short unit suffix matches the bare number on the other side
// and the other way round ("3600s" / "3600 seconds"), because that is a unit
// written two ways rather than two values; two different suffixes on one number
// ("16gb" / "16mb") are still two values.
func lacksValue(from, other map[string]bool) bool {
	for t := range from {
		if other[t] {
			continue
		}
		if core, ok := numericCore(t); ok && other[core] {
			continue
		}
		if isBareNumber(t) && hasUnitOf(other, t) {
			continue
		}
		return true
	}
	return false
}

// hasUnitOf reports whether set holds n with a unit suffix ("300" -> "300s").
func hasUnitOf(set map[string]bool, n string) bool {
	for t := range set {
		if core, ok := numericCore(t); ok && core == n {
			return true
		}
	}
	return false
}

// valueTokens returns the lowercased value-like tokens of s.
//
// The text is split on whitespace, NOT through tokenizeContent: that tokenizer
// splits on every non-alphanumeric rune, which is exactly what erases the shape
// of a version, a path or an identifier. Surrounding punctuation is trimmed
// first, so a sentence-final "everywhere." or a "Redis:" before a clause is a
// word and not a dotted identifier, a trailing '/' is dropped so
// "internal/memory/" is "internal/memory", and a digit-grouping comma is part of
// its number, so "1,000" is "1000". A token is a value when it carries
//
//   - a digit (a version, a port, a count, a commit hash), or
//   - an inner '.', '/', '_', '=', ':', '@', '#' or '\' with at least three
//     letters or digits around it (a path, a module, a host, KEY=value; the
//     floor keeps "e.g" and "a/b" out), or
//   - an upper-case letter after its first rune (camelCase "ingestHandler",
//     ALL-CAPS "TTL"), or
//   - a capital first letter AWAY from a sentence start: a name, "Redis"
//     against "Postgres". A sentence starts at the beginning of the text, at
//     every line, after a leading list or heading marker ("-", "*", "+", "1.",
//     "1)", "#", "##" …, never values themselves) and after '.', '!', '?' or
//     ':'. There a capital is grammar, so "The cache …" against "Cache …" and
//     "## Setup" against nothing are not values, or
//   - a place inside a backticked span: "`make deploy-prod`" is code, so every
//     word of it is a value whatever its case.
//
// A hyphen alone is not a value, so "per-session" and "shipment-event" stay
// words, and neither is a plain lowercase word outside backticks: "uses pgx" and
// "uses sqlx" are the same sentence to this rule (a documented limit, pinned by
// TestStatesDifferentValue).
func valueTokens(s string) map[string]bool {
	out := map[string]bool{}
	for _, f := range valueFields(s) {
		if f.value {
			out[f.word] = true
		}
	}
	return out
}

// valueField is one word of a text as valueTokens reads it, in text order:
// the lowercased word, whether it is a value token, and whether its SHAPE makes
// it one (a digit, an inner path or identifier mark, an inner capital, a name
// away from a sentence start) rather than only its place inside a code span.
type valueField struct {
	word          string
	value, shaped bool
}

// valueFields is valueTokens' reading of s, word by word and in order, list
// markers and empty words dropped. valueTokens is its set of value words;
// lexicalReplacements also aligns two texts' sequences (subjectSwapped).
func valueFields(s string) []valueField {
	var out []valueField
	segments := strings.Split(s, "`")
	sentenceStart, lineStart := true, true
	for si, seg := range segments {
		// An odd segment is inside a span, unless it is the unclosed tail.
		code := si%2 == 1 && si < len(segments)-1
		for li, line := range strings.Split(seg, "\n") {
			if li > 0 {
				sentenceStart, lineStart = true, true
			}
			for _, raw := range strings.Fields(line) {
				if lineStart && !code && isListMarker(raw) {
					continue
				}
				lineStart = false
				tok := strings.Trim(raw, "\"'«»“”‘’()[]{}<>.,;:!?*")
				tok = groupedDigits(strings.TrimRight(tok, "/"))
				starts := sentenceStart
				// The next word starts a sentence after a terminal mark or a
				// colon, whatever this token turned out to be.
				closed := strings.TrimRight(raw, "\"'»”’)]}*")
				sentenceStart = closed != "" && strings.ContainsAny(closed[len(closed)-1:], ".!?:")
				if tok == "" {
					continue
				}
				shaped := isValueToken(tok) || (!starts && isName(tok))
				out = append(out, valueField{word: strings.ToLower(tok), value: code || shaped, shaped: shaped})
			}
		}
	}
	return out
}

// isListMarker reports whether a line's first field is a list or heading
// marker: "-", "*", "+", a run of '#', or a number followed by '.' or ')'.
func isListMarker(f string) bool {
	switch f {
	case "-", "*", "+":
		return true
	}
	if strings.Trim(f, "#") == "" {
		return true
	}
	if len(f) < 2 || (f[len(f)-1] != '.' && f[len(f)-1] != ')') {
		return false
	}
	for _, r := range f[:len(f)-1] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// groupedDigits drops a digit-grouping comma — one between a digit and exactly
// three digits that end the group — so "1,000" and "1000" are one number. A
// comma anywhere else is kept.
func groupedDigits(t string) string {
	if !strings.Contains(t, ",") {
		return t
	}
	isDigit := func(i int) bool { return i >= 0 && i < len(t) && t[i] >= '0' && t[i] <= '9' }
	var b strings.Builder
	for i := 0; i < len(t); i++ {
		if t[i] == ',' && isDigit(i-1) && isDigit(i+1) && isDigit(i+2) && isDigit(i+3) && !isDigit(i+4) {
			continue
		}
		b.WriteByte(t[i])
	}
	return b.String()
}

// isName reports whether tok is a capitalised word: "Redis", "Postgres",
// "Hetzner". Asked only away from a sentence start, where a capital is a name
// and not grammar.
func isName(tok string) bool {
	for _, r := range tok {
		return unicode.IsUpper(r)
	}
	return false
}

func isValueToken(tok string) bool {
	alnum := 0
	inner := false
	upperAfterFirst := false
	for i, r := range tok {
		switch {
		case unicode.IsDigit(r):
			return true
		case unicode.IsLetter(r):
			alnum++
			if i > 0 && unicode.IsUpper(r) {
				upperAfterFirst = true
			}
		case strings.ContainsRune("./_=:@#\\", r):
			inner = true
		}
	}
	return upperAfterFirst || (inner && alnum >= 3)
}

// numericCore splits a number written with a short unit suffix ("3600s",
// "16gb", "500ms") into the number. ok is false for anything else, a version
// ("v4.18.1") or a hash ("4f2a9c1") included.
func numericCore(t string) (string, bool) {
	i := 0
	for i < len(t) && (t[i] >= '0' && t[i] <= '9' || (i > 0 && (t[i] == '.' || t[i] == ',') && i+1 < len(t) && t[i+1] >= '0' && t[i+1] <= '9')) {
		i++
	}
	if i == 0 || i == len(t) {
		return "", false
	}
	suffix := t[i:]
	if len(suffix) > 3 {
		return "", false
	}
	for _, r := range suffix {
		if !unicode.IsLetter(r) && r != '%' {
			return "", false
		}
	}
	return t[:i], true
}

func isBareNumber(t string) bool {
	if t == "" {
		return false
	}
	for _, r := range t {
		if (r < '0' || r > '9') && r != '.' && r != ',' {
			return false
		}
	}
	return t[0] >= '0' && t[0] <= '9'
}
