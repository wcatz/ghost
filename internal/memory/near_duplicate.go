package memory

import (
	"context"
	"fmt"
	"strings"
	"unicode"
)

// dedupStopwords mirrors reflection's stopwords so the two predicates cannot drift.
var dedupStopwords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true,
	"be": true, "by": true, "for": true, "from": true, "in": true, "is": true,
	"it": true, "of": true, "on": true, "or": true, "that": true, "the": true,
	"this": true, "to": true, "with": true,
}

func dedupIsNumericToken(s string) bool {
	if len(s) == 0 {
		return false
	}
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func dedupTokenize(s string) map[string]bool {
	tokens := make(map[string]bool)
	for _, word := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if dedupIsNumericToken(word) {
			tokens[word] = true
			continue
		}
		if len(word) > 1 && !dedupStopwords[word] {
			tokens[word] = true
		}
	}
	return tokens
}

func dedupJaccard(a, b map[string]bool) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1.0
	}
	inter := 0
	for t := range a {
		if b[t] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func dedupContainment(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for t := range a {
		if b[t] {
			inter++
		}
	}
	denom := len(a)
	if len(b) < denom {
		denom = len(b)
	}
	return float64(inter) / float64(denom)
}

func dedupNumericConflict(a, b map[string]bool) bool {
	for t := range a {
		if dedupIsNumericToken(t) && !b[t] {
			return true
		}
	}
	for t := range b {
		if dedupIsNumericToken(t) && !a[t] {
			return true
		}
	}
	return false
}

// AreNearDuplicates reports whether two texts are near-duplicates under the
// assembler's dedup predicate: Jaccard >= 0.5 or full containment, and no
// differing numeric token. It is the one predicate health and the reflect hint
// both read.
func AreNearDuplicates(a, b string) bool {
	ta := dedupTokenize(a)
	tb := dedupTokenize(b)
	sim := dedupJaccard(ta, tb)
	if c := dedupContainment(ta, tb); c == 1.0 {
		sim = 1.0
	}
	if sim < 0.5 {
		return false
	}
	if dedupNumericConflict(ta, tb) {
		return false
	}
	return true
}

// NearDuplicatePairsForProject returns the near-duplicate pairs among the
// live memories of one project, read through the store's current snapshot
// (queryDB), so a diagnostic read does not wait behind a write in flight.
func (s *Store) NearDuplicatePairsForProject(ctx context.Context, projectID string) ([][2]string, error) {
	rows, err := s.queryDB().QueryContext(ctx, `
		SELECT `+memoryColumns+`
		FROM memories
		WHERE project_id = ? AND resolved_at IS NULL
		ORDER BY created_at
	`, projectID)
	if err != nil {
		return nil, fmt.Errorf("near duplicates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	mems, err := scanMemories(rows)
	if err != nil {
		return nil, err
	}
	return NearDuplicatePairs(mems), nil
}

// NearDuplicatePairs returns the near-duplicate pairs among mems, each as an
// unordered id pair with the lower id first. Only memories with ResolvedAt == nil
// are considered; a resolved memory is not withheld as a near-duplicate.
func NearDuplicatePairs(mems []Memory) [][2]string {
	var out [][2]string
	for i := 0; i < len(mems); i++ {
		if mems[i].ResolvedAt != nil {
			continue
		}
		for j := i + 1; j < len(mems); j++ {
			if mems[j].ResolvedAt != nil {
				continue
			}
			if !AreNearDuplicates(mems[i].Content, mems[j].Content) {
				continue
			}
			a, b := mems[i].ID, mems[j].ID
			if a > b {
				a, b = b, a
			}
			out = append(out, [2]string{a, b})
		}
	}
	return out
}
