package reflection

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
)

var numberRe = regexp.MustCompile(`\d+`)

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

func identifiers(text string) map[string]bool {
	found := make(map[string]bool)
	for _, tok := range identifierTokens(text) {
		lower := strings.ToLower(tok)
		if isVersionToken(tok) {
			found[lower] = true
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
	for _, tok := range shaLikeRe.FindAllString(text, -1) {
		if shaLikeToken(tok) {
			found[strings.ToLower(tok)] = true
		}
	}
	return found
}

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

func stripVersionPrefix(tok string) string {
	if len(tok) > 1 && (tok[0] == 'v' || tok[0] == 'V') {
		return tok[1:]
	}
	return tok
}

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

func isPathToken(tok string) bool {
	if strings.HasPrefix(tok, "/") || strings.HasPrefix(tok, "~/") ||
		strings.HasPrefix(tok, "./") || strings.HasPrefix(tok, "../") {
		return true
	}
	return hasFileExtension(tok)
}

func hasFileExtension(tok string) bool {
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
