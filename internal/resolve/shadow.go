// A newer note can retire an imperative, on the repair path only (#698).
package resolve

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/wcatz/ghost/internal/memory"
)

const minSharedIdentifiers = 2

var (
	backtickSpanRe = regexp.MustCompile("`([^`\n]+)`")

	issueRefRe = regexp.MustCompile(`#(\d+)`)

	identifierTokenRe = regexp.MustCompile(`[A-Za-z0-9][A-Za-z0-9_./+-]*`)
)

func keyIdentifiers(content string) map[string]bool {
	canonical := make(map[string]bool)
	for _, tok := range identifierTokenRe.FindAllString(content, -1) {
		tok = strings.TrimRight(tok, ".,")
		if tok == "" {
			continue
		}
		if base, ok := fileBaseName(tok); ok {
			canonical[strings.ToLower(base)] = true
			continue
		}
		if isHostToken(tok) {
			canonical[strings.ToLower(tok)] = true
		}
	}
	found := make(map[string]bool, len(canonical)+2)
	for id := range canonical {
		found[id] = true
	}
	for _, match := range backtickSpanRe.FindAllStringSubmatch(content, -1) {
		span := strings.TrimSpace(match[1])
		if span == "" {
			continue
		}
		if carriesCanonicalIdentifier(span) {
			continue
		}
		found[strings.ToLower(span)] = true
	}
	for _, ref := range issueRefRe.FindAllStringSubmatch(content, -1) {
		found[ref[1]] = true
	}
	return found
}

func carriesCanonicalIdentifier(span string) bool {
	if _, ok := fileBaseName(span); ok {
		return true
	}
	if isHostToken(span) {
		return true
	}
	return issueRefRe.MatchString(span)
}

type subjectNote struct {
	updated     string
	identifiers map[string]bool
}

type subjectIndex map[string]subjectNote

func indexSubjects(mems []memory.Memory) subjectIndex {
	idx := make(subjectIndex, len(mems))
	for _, m := range mems {
		idx[m.ID] = subjectNote{updated: m.UpdatedAt, identifiers: keyIdentifiers(m.Content)}
	}
	return idx
}

func (idx subjectIndex) shadowing(m memory.Memory) (id string, shared int, ok bool) {
	mine := keyIdentifiers(m.Content)
	if len(mine) < minSharedIdentifiers {
		return "", 0, false
	}
	for otherID, other := range idx {
		if !isOlder(m, memory.Memory{ID: otherID, UpdatedAt: other.updated}) {
			continue
		}
		n := 0
		for ident := range mine {
			if other.identifiers[ident] {
				n++
			}
		}
		if n >= minSharedIdentifiers && (!ok || otherID < id) {
			id, shared, ok = otherID, n, true
		}
	}
	return id, shared, ok
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

func fileBaseName(tok string) (string, bool) {
	base := tok
	if idx := strings.LastIndex(tok, "/"); idx >= 0 {
		base = tok[idx+1:]
	}
	idx := strings.LastIndex(base, ".")
	if idx < 0 {
		return "", false
	}
	ext := base[idx+1:]
	if len(ext) < 2 || len(ext) > 8 || !isAlphaNum(ext) {
		return "", false
	}
	return base, true
}

func isAlpha(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsLetter(r) {
			return false
		}
	}
	return true
}

func isAlphaNum(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}
