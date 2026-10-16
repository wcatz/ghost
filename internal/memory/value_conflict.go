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
// word and not a dotted identifier. A token is a value when it carries
//
//   - a digit (a version, a port, a count, a commit hash), or
//   - an inner '.', '/', '_', '=', ':', '@', '#' or '\' with at least three
//     letters or digits around it (a path, a module, a host, KEY=value; the
//     floor keeps "e.g" and "a/b" out), or
//   - an upper-case letter after its first rune (camelCase "ingestHandler",
//     ALL-CAPS "TTL").
//
// A plain capitalised word is not a value: a sentence-initial "The" or a
// restatement that starts "Cache …" where the other starts "The cache …" would
// otherwise refuse every reworded fact. A hyphen alone is not either, so
// "per-session" and "shipment-event" stay words.
func valueTokens(s string) map[string]bool {
	out := map[string]bool{}
	for _, raw := range strings.Fields(s) {
		tok := strings.Trim(raw, "\"'`«»“”‘’()[]{}<>.,;:!?*")
		if tok == "" || !isValueToken(tok) {
			continue
		}
		out[strings.ToLower(tok)] = true
	}
	return out
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
