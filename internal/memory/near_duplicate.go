package memory

import (
	"context"
	"fmt"
	"strings"
	"unicode"
)

const maxNearDuplicateScanRows = MaxConsolidationInput

// MaxNearDuplicateScanRows bounds a near-duplicate scan.
const MaxNearDuplicateScanRows = maxNearDuplicateScanRows

// MaxConsolidationInput bounds consolidation input.
const MaxConsolidationInput = 2000

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

// DedupTokenize tokenizes text for near-duplicate comparison.
func DedupTokenize(s string) map[string]bool { return dedupTokenize(s) }

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

// AreNearDuplicates reports whether two texts are near duplicates.
func AreNearDuplicates(a, b string) bool {
	ta := dedupTokenize(a)
	tb := dedupTokenize(b)
	return AreNearDuplicatesTokens(ta, tb)
}

// AreNearDuplicatesTokens reports whether two token sets are near duplicates.
func AreNearDuplicatesTokens(a, b map[string]bool) bool {
	sim := dedupJaccard(a, b)
	if c := dedupContainment(a, b); c == 1.0 {
		sim = 1.0
	}
	if sim < 0.5 {
		return false
	}
	if dedupNumericConflict(a, b) {
		return false
	}
	return true
}

var (
	AreNearDuplicatesTokenSets     = AreNearDuplicatesTokens
	AreNearDuplicatesSets          = AreNearDuplicatesTokens
	AreNearDuplicatesFromTokens    = AreNearDuplicatesTokens
	AreNearDuplicatesFromTokenSets = AreNearDuplicatesTokens
	AreNearDuplicateTokenSets      = AreNearDuplicatesTokens
	AreNearDuplicatesTokenized     = AreNearDuplicatesTokens
	AreNearDuplicatesWithTokens    = AreNearDuplicatesTokens
	NearDuplicatesFromTokens       = AreNearDuplicatesTokens
	NearDuplicateTokenSets         = AreNearDuplicatesTokens
)

// NearDuplicatePairsForProject returns near-duplicate pairs among consolidatable memories in a project.
func (s *Store) NearDuplicatePairsForProject(ctx context.Context, projectID string) ([][2]string, bool, error) {
	rows, err := s.queryDB().QueryContext(ctx, `
		SELECT `+memoryColumns+`
		FROM memories
		WHERE project_id = ? AND resolved_at IS NULL
		ORDER BY created_at
	`, projectID)
	if err != nil {
		return nil, false, fmt.Errorf("near duplicates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	mems, err := scanMemories(rows)
	if err != nil {
		return nil, false, err
	}
	var live []Memory
	for _, m := range mems {
		if !IsConsolidatable(m) {
			continue
		}
		live = append(live, m)
	}
	truncated := false
	if len(live) > maxNearDuplicateScanRows {
		truncated = true
		live = live[:maxNearDuplicateScanRows]
	}
	tokens := make([]map[string]bool, len(live))
	for i := range live {
		tokens[i] = dedupTokenize(live[i].Content)
	}
	var out [][2]string
	for i := 0; i < len(live); i++ {
		if err := ctx.Err(); err != nil {
			return out, truncated, err
		}
		for j := i + 1; j < len(live); j++ {
			if j%64 == 0 {
				if err := ctx.Err(); err != nil {
					return out, truncated, err
				}
			}
			if !AreNearDuplicatesTokens(tokens[i], tokens[j]) {
				continue
			}
			a, b := live[i].ID, live[j].ID
			if a > b {
				a, b = b, a
			}
			out = append(out, [2]string{a, b})
		}
	}
	return out, truncated, nil
}

// NearDuplicatePairs returns near-duplicate pairs among consolidatable memories.
func NearDuplicatePairs(mems []Memory) [][2]string {
	var live []Memory
	for _, m := range mems {
		if !IsConsolidatable(m) {
			continue
		}
		live = append(live, m)
	}
	if len(live) > maxNearDuplicateScanRows {
		live = live[:maxNearDuplicateScanRows]
	}
	tokens := make([]map[string]bool, len(live))
	for i := range live {
		tokens[i] = dedupTokenize(live[i].Content)
	}
	var out [][2]string
	for i := 0; i < len(live); i++ {
		for j := i + 1; j < len(live); j++ {
			if !AreNearDuplicatesTokens(tokens[i], tokens[j]) {
				continue
			}
			a, b := live[i].ID, live[j].ID
			if a > b {
				a, b = b, a
			}
			out = append(out, [2]string{a, b})
		}
	}
	return out
}
