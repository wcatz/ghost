package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
)

func lexicalReplacements(rows []Candidate, edges []LinkEdge, now time.Time) map[string][]string {
	type party struct {
		c       *Candidate
		created time.Time
		tokens  map[string]bool
		values  map[string]bool
		fields  []valueField
	}
	parties := make([]party, 0, len(rows))
	for i := range rows {
		c := &rows[i]
		if c.ResolvedAt != nil {
			continue
		}
		switch state, _ := ValidityState(c.ValidFrom, c.ValidUntil, c.VerifiedAt, now); state {
		case ValidityExpired, ValidityFuture:
			continue
		}
		created, ok := ParseStamp(c.CreatedAt)
		if !ok {
			continue
		}
		tokens := tokenizeContent(c.Content)
		if len(tokens) == 0 {
			continue
		}
		fields := valueFields(c.Content)
		values := map[string]bool{}
		for _, f := range fields {
			if f.value {
				values[f.word] = true
			}
		}
		parties = append(parties, party{c: c, created: created, tokens: tokens, values: values, fields: fields})
	}
	if len(parties) < 2 {
		return nil
	}
	explicit := map[[2]string]bool{}
	for _, e := range edges {
		if e.Relation != "supersedes" && e.Relation != "contradicts" {
			continue
		}
		explicit[[2]string{e.From, e.To}] = true
		explicit[[2]string{e.To, e.From}] = true
	}
	var out map[string][]string
	created := map[string]time.Time{}
	for i := range parties {
		for j := i + 1; j < len(parties); j++ {
			a, b := parties[i], parties[j]
			if a.c.ID == b.c.ID || a.c.ProjectID != b.c.ProjectID || !scopesEqual(a.c.Scope, b.c.Scope) {
				continue
			}
			if a.created.Equal(b.created) || explicit[[2]string{a.c.ID, b.c.ID}] {
				continue
			}
			if mergeScore(a.tokens, b.tokens) <= 0 {
				continue
			}
			older, newer := a, b
			if b.created.Before(a.created) {
				older, newer = b, a
			}
			swapped := sharesValue(a.values, b.values) && lacksValue(a.values, b.values) && lacksValue(b.values, a.values)
			if !swapped && !retiresFields(newer.fields, older.fields, older.values) {
				continue
			}
			if subjectSwapped(a.fields, b.fields) {
				continue
			}
			if out == nil {
				out = map[string][]string{}
			}
			out[older.c.ID] = append(out[older.c.ID], newer.c.ID)
			created[newer.c.ID] = newer.created
		}
	}
	for id, newer := range out {
		sort.SliceStable(newer, func(x, y int) bool {
			if !created[newer[x]].Equal(created[newer[y]]) {
				return created[newer[x]].After(created[newer[y]])
			}
			return newer[x] < newer[y]
		})
		out[id] = newer
	}
	return out
}

func subjectSwapped(a, b []valueField) bool {
	for len(a) > 0 && len(b) > 0 && a[0].word == b[0].word {
		a, b = a[1:], b[1:]
	}
	for len(a) > 0 && len(b) > 0 && a[len(a)-1].word == b[len(b)-1].word {
		a, b = a[:len(a)-1], b[:len(b)-1]
	}
	if len(a) > maxAlignedWords || len(b) > maxAlignedWords {
		return true
	}
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i].word == b[j].word {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	i, j := 0, 0
	si, sj := 0, 0
	flush := func() bool {
		if i-si != 1 || j-sj != 1 {
			return false
		}
		return subjectWord(a[si]) && subjectWord(b[sj])
	}
	for i < n && j < m {
		switch {
		case a[i].word == b[j].word:
			if flush() {
				return true
			}
			i++
			j++
			si, sj = i, j
		case lcs[i+1][j] >= lcs[i][j+1]:
			i++
		default:
			j++
		}
	}
	i, j = n, m
	return flush()
}

const maxAlignedWords = 64

func subjectWord(f valueField) bool {
	if f.shaped {
		return false
	}
	letters := 0
	for _, r := range f.word {
		if unicode.IsLetter(r) {
			letters++
		}
	}
	return letters >= 4 && !functionWords[f.word]
}

var functionWords = map[string]bool{
	"also": true, "been": true, "does": true, "from": true, "have": true, "into": true,
	"just": true, "longer": true, "more": true, "much": true, "must": true, "never": true,
	"only": true, "once": true, "should": true, "since": true, "still": true, "than": true,
	"that": true, "then": true, "there": true, "these": true, "they": true, "this": true,
	"those": true, "under": true, "until": true, "were": true, "what": true, "when": true,
	"where": true, "which": true, "while": true, "will": true, "with": true, "would": true,
	"always": true, "after": true, "before": true, "every": true,
}

func sharesValue(a, b map[string]bool) bool {
	for t := range a {
		if b[t] {
			return true
		}
	}
	return false
}

func scopesEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

func replacementPairsWithin(ctx context.Context, db Queryer, rows []Memory, now time.Time) (map[[2]string]bool, error) {
	repl, err := replacementsWithin(ctx, db, rows, now)
	if err != nil {
		return nil, err
	}
	return replacementPairKeys(repl), nil
}

func replacementsWithin(ctx context.Context, db Queryer, rows []Memory, now time.Time) (map[string][]string, error) {
	if len(rows) < 2 {
		return nil, nil
	}
	ids := make([]string, len(rows))
	cands := make([]Candidate, len(rows))
	for i, m := range rows {
		ids[i] = m.ID
		cands[i] = Candidate{Memory: m}
	}
	edges, err := explicitEdgesWithin(ctx, db, ids)
	if err != nil {
		return nil, err
	}
	return lexicalReplacements(cands, edges, now), nil
}

func replacementPairKeys(repl map[string][]string) map[[2]string]bool {
	if len(repl) == 0 {
		return nil
	}
	out := make(map[[2]string]bool, 2*len(repl))
	for older, newer := range repl {
		for _, n := range newer {
			out[[2]string{older, n}] = true
			out[[2]string{n, older}] = true
		}
	}
	return out
}

func explicitEdgesWithin(ctx context.Context, db Queryer, ids []string) ([]LinkEdge, error) {
	ph := make([]string, len(ids))
	args := make([]interface{}, 0, len(ids)*2)
	for i, id := range ids {
		ph[i] = "?"
		args = append(args, id)
	}
	for _, id := range ids {
		args = append(args, id)
	}
	list := strings.Join(ph, ",")
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT l.source_id, l.target_id, l.relation
		FROM memory_links l
		WHERE l.relation IN ('supersedes', 'contradicts') AND l.invalidated_at IS NULL
		  AND l.source_id IN (%s) AND l.target_id IN (%s)
	`, list, list), args...)
	if err != nil {
		return nil, fmt.Errorf("replacement pairs: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var out []LinkEdge
	for rows.Next() {
		var e LinkEdge
		if err := rows.Scan(&e.From, &e.To, &e.Relation); err != nil {
			return nil, fmt.Errorf("replacement pairs: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("replacement pairs: %w", err)
	}
	return out, nil
}

func withoutReplacementPairs(pairs []demotionPairs, repl map[[2]string]bool) []demotionPairs {
	if len(repl) == 0 {
		return pairs
	}
	return vetoLosers(pairs, func(pr demotionPairs) bool { return repl[[2]string{pr.loser, pr.winner}] })
}

func withoutReplacingLosers(pairs []demotionPairs, repl map[string][]string) []demotionPairs {
	if len(repl) == 0 {
		return pairs
	}
	replacer := make(map[string]bool, len(repl))
	for _, newer := range repl {
		for _, n := range newer {
			replacer[n] = true
		}
	}
	return vetoLosers(pairs, func(pr demotionPairs) bool { return replacer[pr.loser] })
}

func replacementRemovalVetoes(ctx context.Context, db Queryer, pairs []demotionPairs, rows []Memory, now time.Time) ([]demotionPairs, error) {
	if len(pairs) == 0 {
		return pairs, nil
	}
	repl, err := replacementsWithin(ctx, db, rows, now)
	if err != nil {
		return pairs, err
	}
	pairs = withoutReplacementPairs(pairs, replacementPairKeys(repl))
	return withoutReplacingLosers(pairs, repl), nil
}

func exemptReplacementPairs(ctx context.Context, db Queryer, pairs []demotionPairs, rows []Memory, now time.Time) ([]demotionPairs, error) {
	if len(pairs) == 0 {
		return pairs, nil
	}
	project := make(map[string]string, len(rows))
	for _, m := range rows {
		project[m.ID] = m.ProjectID
	}
	party := make(map[string]bool, 2*len(pairs))
	for _, pr := range pairs {
		lp, lok := project[pr.loser]
		wp, wok := project[pr.winner]
		if !lok || !wok || lp != wp {
			continue
		}
		party[pr.loser], party[pr.winner] = true, true
	}
	if len(party) < 2 {
		return pairs, nil
	}
	subset := make([]Memory, 0, len(party))
	for _, m := range rows {
		if party[m.ID] {
			subset = append(subset, m)
			delete(party, m.ID)
		}
	}
	repl, err := replacementPairsWithin(ctx, db, subset, now)
	if err != nil {
		return pairs, err
	}
	return withoutReplacementPairs(pairs, repl), nil
}
