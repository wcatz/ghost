package memory

import (
	"sort"
	"strings"
)

// Term selection for the FTS5 query sanitizer (sanitizeFTSN).
//
// WHY this exists: sanitizeFTSN may emit at most maxWords terms
// (ftsSearchWordLimit for search), and the cap must not grow — measured,
// see the ftsSearchWordLimit comment. The old behavior truncated
// POSITIONALLY (first maxWords), and a natural-language query's first words
// are its leading function words, while its TAIL is where the specific,
// discriminative terms live ("... pin the ghost_windows_arm64 sha256
// digest"). Positional truncation therefore systematically discarded exactly
// the terms most likely to match the intended memory. Selection ranks terms
// by value instead, keeps the top maxWords, and emits them in original query
// order — the OR query broadens nowhere; only WHICH terms fill the fixed
// budget changes.
//
// Scoring is deliberately per-term, corpus-free and dependency-free: this
// runs per query on the search hot path, so no index/statistics lookups, no
// external word lists.
//
//   - ftsTermValueStopword (0): English function words (ftsStopwords),
//     matched on the lowercased term so casing never rescues filler
//     ("AND" is still a stopword).
//   - ftsTermValueContent (1): an ordinary content word.
//   - ftsTermValueIdentifier (2): identifier-shaped tokens (see
//     isIdentifierTerm) — mixed alnum, camelCase/snake/kebab/path shapes,
//     version/IP-like runs, ALL-CAPS acronyms, bare numerics. These are
//     exactly the terms that make a query's tail specific.
//
// Selection rule: rank by value descending, ties broken by ORIGINAL POSITION
// (stable). The selected terms are then re-sorted into original query order
// before joining — OR-query semantics don't care about order, but stable
// output keeps tests and debug output readable. Stopwords are only dropped
// when at least maxWords higher-value terms exist; otherwise they fill the
// cap. Selection therefore never emits fewer than min(len(terms), maxWords)
// terms (pinned by TestSanitizeFTSN_StopwordSelectionRules).

const (
	// ftsTermValueStopword is the lowest value tier: filler that only
	// reaches the query when better terms can't fill the cap.
	ftsTermValueStopword = iota
	// ftsTermValueContent is an ordinary content word.
	ftsTermValueContent
	// ftsTermValueIdentifier is a high-value identifier-shaped token.
	ftsTermValueIdentifier
)

// ftsStopwords is the low-value tier for FTS term selection: English
// function words (articles, pronouns, prepositions, auxiliaries, wh-words,
// politeness filler) carrying no retrieval signal in a natural-language
// query.
//
// Provenance: a superset of the repo's existing stopword list,
// internal/reflection's `stopwords` map (tier_sqlite.go), which cannot be
// imported here because reflection imports internal/memory — the dependency
// runs the other way — so the set is embedded and kept aligned by this
// comment. It is intentionally larger than reflection's 21-word list:
// reflection filters tokens inside a similarity score where every dropped
// word shifts Jaccard, while selection only DEMOTES a term and never below
// the fill rule, so a broader function-word tier is low-risk here.
//
// Deliberately excluded: number words ("port 8" vs "port 9" must differ —
// numerics are signal; TestSanitizeFTS also pins "one".."twelve" as content)
// and negators ("not"/"no" change query meaning).
//
// Matching lowercases the term; the emitted term always keeps its original
// text (selection changes which terms are emitted, never how they are
// escaped or cased).
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

// ftsTerm is one sanitized term with its selection metadata.
type ftsTerm struct {
	text  string // emitted, escaped form (unchanged by selection)
	value int    // ftsTermValue*
	pos   int    // original position among cleaned terms
}

// ftsTermValue scores a cleaned (pre-escape) term for selection. See the
// file-level comment for the WHY; order matters: stopword is checked first
// so casing can never promote filler (a shouted "AND" is still a stopword).
func ftsTermValue(clean string) int {
	if ftsStopwords[strings.ToLower(clean)] {
		return ftsTermValueStopword
	}
	if isIdentifierTerm(clean) {
		return ftsTermValueIdentifier
	}
	return ftsTermValueContent
}

// isIdentifierTerm reports whether w looks like an identifier rather than an
// ordinary word. Matched shapes:
//
//   - mixed letters+digits: sha256, arm64, sev1, log2 — and hex-ish tokens
//     with digits (a digitless hex string like "deadbeef" overlaps English
//     words — facade, decade — too far to match safely);
//   - bare numerics: 8080, 300 ("port 8" is signal, see ftsStopwords);
//   - camelCase boundaries: sanitizeFTSN, SearchFTS;
//   - snake/kebab/path separators: ghost_windows_arm64, sealed-secrets,
//     fix/fts-term-selection;
//   - version/IP-like digit runs: 1.2.3, 192.168.9.150;
//   - ALL-CAPS acronyms (>= 2 letters): NDCG, SHA.
//
// All checks are ASCII: sanitizeFTSN's edge trim already strips non-ASCII
// from term edges, and the corpus/query vocabulary is code-domain English.
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

// selectFTSTERMs returns the top maxWords terms by value (ties by original
// position), re-sorted into original query order. terms must be longer than
// maxWords; shorter inputs are returned untouched.
func selectFTSTERMs(terms []ftsTerm, maxWords int) []ftsTerm {
	if len(terms) <= maxWords {
		return terms
	}
	sel := make([]ftsTerm, len(terms))
	copy(sel, terms)
	// Stable sort keeps ties (equal value) in original position order.
	sort.SliceStable(sel, func(i, j int) bool { return sel[i].value > sel[j].value })
	sel = sel[:maxWords]
	// Restore original query order for emission.
	sort.SliceStable(sel, func(i, j int) bool { return sel[i].pos < sel[j].pos })
	return sel
}
