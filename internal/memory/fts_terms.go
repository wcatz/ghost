package memory

import (
	"log/slog"
	"sort"
	"strings"
)

const (
	ftsTermValueStopword = iota

	ftsTermValueContent

	ftsTermValueIdentifier
)

var ftsStopwords = map[string]bool{
	"a": true, "about": true, "after": true, "also": true, "am": true,
	"an": true, "and": true, "are": true, "as": true, "at": true,
	"be": true, "been": true, "before": true, "being": true,
	"between": true, "but": true, "by": true,
	"can": true, "could": true,
	"did": true, "do": true, "does": true, "doing": true, "done": true,
	"during": true,
	"each":   true,
	"for":    true, "from": true,
	"had": true, "has": true, "have": true, "he": true, "her": true,
	"here": true, "hers": true, "him": true, "his": true, "how": true,
	"i": true, "if": true, "in": true, "into": true, "is": true,
	"it": true, "its": true,
	"may": true, "me": true, "might": true, "must": true, "my": true,
	"of": true, "on": true, "onto": true, "or": true, "our": true,
	"ours": true,
	"per":  true, "please": true,
	"she": true, "should": true, "so": true,
	"than": true, "that": true, "the": true, "their": true,
	"theirs": true, "them": true, "then": true, "there": true,
	"these": true, "they": true, "this": true, "those": true,
	"to": true, "too": true,
	"under": true, "until": true,
	"very": true,
	"was":  true, "we": true, "were": true, "what": true, "when": true,
	"where": true, "which": true, "while": true, "who": true,
	"whom": true, "whose": true, "why": true, "will": true,
	"with": true, "would": true,
	"you": true, "your": true, "yours": true,
}

type ftsTerm struct {
	text   string // emitted, escaped form (unchanged by selection)
	clean  string // the term as the tokenizer will see it
	value  int    // ftsTermValue*
	pos    int    // original position among cleaned terms
	prefix bool   // the word carried a trailing `*`
}

func ftsTermValue(clean string) int {
	if ftsStopwords[strings.ToLower(clean)] {
		return ftsTermValueStopword
	}
	if isIdentifierTerm(clean) {
		return ftsTermValueIdentifier
	}
	return ftsTermValueContent
}

func isIdentifierTerm(w string) bool {
	hasLetter, hasDigit := false, false
	allCaps := true
	letterCount := 0
	prevLowerASCII := false
	camelBoundary := false
	for i := 0; i < len(w); i++ {
		c := w[i]
		isLower := c >= 'a' && c <= 'z'
		isUpper := c >= 'A' && c <= 'Z'
		isDigit := c >= '0' && c <= '9'
		if isLower || isUpper {
			hasLetter = true
			letterCount++
			if !isUpper {
				allCaps = false
			}
		} else {
			allCaps = false
		}
		if isDigit {
			hasDigit = true
		}
		if prevLowerASCII && isUpper {
			camelBoundary = true
		}
		prevLowerASCII = isLower
	}
	switch {
	case hasLetter && hasDigit: // mixed alnum: sha256, sev1, v1.2.3, hex-with-digits
		return true
	case !hasLetter && hasDigit: // bare numeric / version / IP: 8080, 1.2.3, 192.168.9.150
		return true
	case camelBoundary: // camelCase: sanitizeFTSN, SearchFTS
		return true
	case strings.ContainsAny(w, "-_/"): // snake/kebab/path: ghost_windows_arm64, fix/x
		return true
	case allCaps && letterCount >= 2: // acronym: NDCG, SHA
		return true
	}
	return false
}

func selectFTSTERMs(terms []ftsTerm, maxWords int) []ftsTerm {
	if len(terms) <= maxWords {
		return terms
	}
	sel := make([]ftsTerm, len(terms))
	copy(sel, terms)

	sort.SliceStable(sel, func(i, j int) bool { return sel[i].value > sel[j].value })
	sel = sel[:maxWords]

	sort.SliceStable(sel, func(i, j int) bool { return sel[i].pos < sel[j].pos })
	return sel
}

// QueryTerm is one term of a keyword query as the sanitizer reads it, for a

type QueryTerm struct {
	Text string

	Identifier bool
}

const (
	maxDistinctiveScanBytes = 8 << 10
	maxDistinctiveScanTerms = 4096
)

// DistinctiveTerms returns the terms of text that can carry a keyword match:

func DistinctiveTerms(text string, max int, logger *slog.Logger) []QueryTerm {
	if max <= 0 {
		return nil
	}
	if len(text) > maxDistinctiveScanBytes {
		text = text[:maxDistinctiveScanBytes]
	}
	var ids, words []QueryTerm
	seen := make(map[string]bool)
	for _, t := range ftsQueryTerms(text, maxDistinctiveScanTerms, logger) {
		if t.value == ftsTermValueStopword {
			continue
		}
		key := strings.ToLower(t.clean)
		if seen[key] {
			continue
		}
		seen[key] = true
		q := QueryTerm{Text: t.clean, Identifier: t.value == ftsTermValueIdentifier || isFileNameTerm(t.clean)}
		if q.Identifier {
			ids = append(ids, q)
		} else {
			words = append(words, q)
		}
	}
	out := append(ids, words...)
	if len(out) > max {
		out = out[:max]
	}
	return out
}

func wordYieldsTerm(word string) bool {
	clean := strings.TrimFunc(word, ftsEdgeRuneTrimmed)
	return ftsHasBaseRune(clean)
}

// HasFTSTerm reports whether text yields at least one extractable term under

func HasFTSTerm(text string) bool {
	for _, word := range strings.Fields(text) {
		if wordYieldsTerm(word) {
			return true
		}
	}
	return false
}

func isFileNameTerm(w string) bool {
	i := strings.IndexByte(w, '.')
	if i <= 0 || i >= len(w)-1 {
		return false
	}
	alnum := func(c byte) bool {
		return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
	}
	return alnum(w[i-1]) && alnum(w[i+1])
}
