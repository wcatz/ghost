package reflection

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Identifier extraction for the grounding check (#639). A consolidation run
// cannot learn anything specific that is not in its input, so an identifier a
// merge or rewrite introduces is by definition one the model made up or mangled.
// The classes below are the ones the maintenance benchmark caught being
// corrupted while the model was only supposed to be copying a memory:
// 2.BeXIAhbj.js became 2.BeXIAhbq.js, a testnet fact was credited to mainnet, a
// "/keys" path and a pkill stop command were quietly dropped from the text that
// replaced it.
//
// Ordinary prose is deliberately not a class. Rejecting a merge for a new
// connective word would reject every honest merge, and the cost of that is a
// corpus that can never be consolidated. Only a wrong specific is
// unrecoverable, because it is stored as fact. A bare proper noun the model
// misspells ("Jaffney" for "Jaffrey") is outside these classes too: catching
// it needs a dictionary of every name a project uses, and a rule loose enough
// to catch it would reject merges over ordinary capitalized words.
//
// Every identifier is compared lowercased: a host or path restated with
// different capitalisation is the same identifier, not a new one.

// numberRe pulls the bare numbers out of a token — ports, versions, counts,
// years, and the digit runs inside a hash or a version.
var numberRe = regexp.MustCompile(`\d+`)

// identifierTokens splits text into the runs of characters an identifier can
// be made of. Everything else is a separator, so "port 2222, not 22" yields
// three tokens and a path keeps its slashes. Trailing sentence punctuation is
// trimmed: "/keys/relay3." and "/keys/relay3" name the same path, and a full
// stop that changed an identifier's identity would reject a faithful merge.
func identifierTokens(text string) []string {
	raw := strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) &&
			!strings.ContainsRune("/.-_~+@", r)
	})
	tokens := make([]string, 0, len(raw))
	for _, tok := range raw {
		if tok = strings.TrimRight(tok, ".,"); tok != "" {
			tokens = append(tokens, tok)
		}
	}
	return tokens
}

// identifiers returns the identifier-shaped tokens in text, lowercased and
// deduped.
func identifiers(text string) map[string]bool {
	found := make(map[string]bool)
	for _, tok := range identifierTokens(text) {
		lower := strings.ToLower(tok)
		if isVersionToken(tok) {
			found[lower] = true
			// Both spellings of a pinned version are the same identifier: a
			// source that wrote v0.204.3 grounds an output that writes 0.204.3,
			// and the digit runs below are in the set either way.
			if stripped := stripVersionPrefix(lower); stripped != lower {
				found[stripped] = true
			}
		}
		if isHostToken(tok) || isPathToken(tok) {
			found[lower] = true
		}
		for _, num := range numberRe.FindAllString(tok, -1) {
			found[num] = true
		}
	}
	// Hash-shaped tokens come from the fabrication guard's own rule (shaLikeRe +
	// shaLikeToken) rather than a second definition, so "what counts as a commit
	// SHA" has one answer in this package. It scans the raw text because a hash
	// is usually part of a longer token — a branch name, a file, an id — that the
	// rules above see as one path or host and not as a hash.
	for _, tok := range shaLikeRe.FindAllString(text, -1) {
		if shaLikeToken(tok) {
			found[strings.ToLower(tok)] = true
		}
	}
	return found
}

// isVersionToken reports a dotted-decimal: 1.2, 1.2.3, 0.204.3, and the
// v-prefixed spelling of each. At least two numeric segments, so a bare port is
// a number and not this.
func isVersionToken(tok string) bool {
	parts := strings.Split(stripVersionPrefix(tok), ".")
	if len(parts) < 2 {
		return false
	}
	for _, p := range parts {
		if !isNumericToken(p) {
			return false
		}
	}
	return true
}

// stripVersionPrefix drops the "v" of a v-prefixed version, leaving anything
// else — including a bare "v2" label, which is a name and not a version.
func stripVersionPrefix(tok string) string {
	if len(tok) > 1 && (tok[0] == 'v' || tok[0] == 'V') {
		return tok[1:]
	}
	return tok
}

// isHostToken reports a dotted name whose last label is alphabetic and at least
// two characters: node-3.example.com, fsn1.example.net, node.js. The length
// floor is what keeps "i.e" and "e.g" out — prose is full of them, and a host
// rule that flagged them would reject honest merges. A purely numeric tail is a
// number, not a host, which is also what keeps 192.168.1.1 and 0.204.3 out.
func isHostToken(tok string) bool {
	parts := strings.Split(tok, ".")
	if len(parts) < 2 || len(parts[len(parts)-1]) < 2 || !isAlpha(parts[len(parts)-1]) {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
	}
	return true
}

// isPathToken reports a filesystem path. Rooted or explicitly relative paths
// count as soon as they start that way; anything else has to carry a file
// extension, which is what separates internal/reflection/prompt.go and
// 2.BeXIAhbj.js from and/or, read/write and input/output — prose joins those
// constantly, and a path rule that flagged them would reject honest merges.
// The extension needs at least two characters for the same reason
// isHostToken's last label does: "e.g" is not a file.
func isPathToken(tok string) bool {
	if strings.HasPrefix(tok, "/") || strings.HasPrefix(tok, "~/") ||
		strings.HasPrefix(tok, "./") || strings.HasPrefix(tok, "../") {
		return true
	}
	return hasFileExtension(tok)
}

func hasFileExtension(tok string) bool {
	// Only the final segment's last dot can be the extension: "v1.2" is a
	// version, and a directory named "pkg.io" inside a longer path must not
	// supply the extension.
	last := tok
	if idx := strings.LastIndex(tok, "/"); idx >= 0 {
		last = tok[idx+1:]
	}
	idx := strings.LastIndex(last, ".")
	if idx < 0 {
		return false
	}
	ext := last[idx+1:]
	return len(ext) >= 2 && len(ext) <= 8 && isAlphaNum(ext)
}

func isAlpha(s string) bool {
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return len(s) > 0
}

func isAlphaNum(s string) bool {
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return len(s) > 0
}

// unknownIdentifiers returns the identifiers in out that appear in none of
// sources, sorted so the diagnostic is stable. Sources are the memories a merge
// or rewrite drew on: an identifier in ANY of them grounds the output, because a
// merge legitimately carries a specific across from a sibling.
func unknownIdentifiers(out string, sources []string) []string {
	grounded := make(map[string]bool)
	for _, s := range sources {
		for id := range identifiers(s) {
			grounded[id] = true
		}
	}
	var unknown []string
	for id := range identifiers(out) {
		if !grounded[id] {
			unknown = append(unknown, id)
		}
	}
	sort.Strings(unknown)
	return unknown
}
