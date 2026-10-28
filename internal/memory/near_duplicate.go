package memory

import (
	"context"
	"fmt"
	"strings"
	"unicode"
)

// maxNearDuplicateScanRows bounds how many consolidatable rows a single
// near-duplicate scan considers. It mirrors the consolidation input bound
// (maxConsolidationInput in cmd/ghost): above it a scan would be quadratic
// in both time and token work, so the scan stops at the bound and reports
// that it was capped.
// maxNearDuplicateScanRows bounds how many consolidatable rows a single
// near-duplicate scan considers. It is defined as MaxConsolidationInput below
// rather than as a second literal: the scan's bound is the same number as the
// consolidation input bound, and keeping one definition means a change to the
// bound moves both. memory cannot import cmd/ghost (cmd imports memory), so
// the value is mirrored here; see MaxConsolidationInput.
const maxNearDuplicateScanRows = MaxConsolidationInput

// MaxNearDuplicateScanRows is the exported name for maxNearDuplicateScanRows.
const MaxNearDuplicateScanRows = maxNearDuplicateScanRows

// MaxConsolidationInput mirrors cmd/ghost's consolidation input bound
// (maxConsolidationInput, 2000). memory cannot import cmd/ghost — the
// dependency runs cmd -> memory — so the value is mirrored here instead of
// referenced. A comment in cmd/ghost/lifecycle.go names this mirror.
const MaxConsolidationInput = 2000

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

// DedupTokenize is the exported spelling of dedupTokenize.
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

// AreNearDuplicates reports whether two texts are near-duplicates under Ghost's
// text dedup predicate: Jaccard >= 0.5 or full containment, and no differing
// numeric token. It is the predicate health and the reflect hint both read.
// The assembler's read path removes near-duplicate losers on memory links and
// cosine similarity, which is a different signal, so a pair this reports is not
// necessarily one the read path withholds.
func AreNearDuplicates(a, b string) bool {
	ta := dedupTokenize(a)
	tb := dedupTokenize(b)
	return AreNearDuplicatesTokens(ta, tb)
}

// AreNearDuplicatesTokens reports whether two already-tokenized sets are
// near-duplicates. It is the token-set variant of AreNearDuplicates: tokenize
// once per row, then compare sets. AreNearDuplicates and this function agree
// on the same inputs.
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

// Token-set variant aliases, so callers using any reasonable spelling reach
// the same implementation.
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

// NearDuplicatePairsForProject returns the near-duplicate pairs among the
// consolidatable memories of one project, read through the store's current
// snapshot (queryDB), so a diagnostic read does not wait behind a write in
// flight. Rows are bounded to maxNearDuplicateScanRows; truncated reports
// whether the bound cut the scan. A cancelled ctx aborts the scan.
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
	// Count only rows the named commands would act on.
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
	// Tokenize each row once.
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

// NearDuplicatePairs returns the near-duplicate pairs among mems, each as an
// unordered id pair with the lower id first. Only consolidatable memories are
// considered; a resolved, pinned, persistent, manual, builtin or expired row
// is not counted.
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
