package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
)

// lexicalReplacements names, for each row of one retrieval, the LATER rows of
// the same retrieval that look like its replacement: a newer note about the same
// subject that states a different value of it. It is the read-side counterpart
// of the save path's value guard (statesDifferentValue). A save that swaps a
// value is no longer folded into the claim it replaces, so the store holds the
// two as unlinked rows, and with no lifecycle pass between sessions (the
// default install) nothing else relates them. The passive order is importance
// times decay, which can put the retired claim first.
//
// A pair is the pair the save path would have FOLDED but refused because the
// value differs, and that NAMES THE SAME THING. Every similarity and value test
// is one the save path already makes, so nothing here is a new metric; the one
// addition is the shared value token below, read from the same valueTokens:
//
//   - the same project, and EQUAL scope maps. Two rows whose scopes differ are
//     two settings (staging and production), never a replacement. That is
//     stricter than scopesConflict, which lets a scoped row and an unscoped one
//     stand together; a value that holds in one environment does not retire the
//     general claim.
//   - similarity at the save path's own fold bar: mergeScore > 0 within one
//     category (the same-category probe), token Jaccard >=
//     upsertCrossCategoryThreshold across categories (the cross-category probe).
//   - statesDifferentValue: each side holds a value token the other lacks.
//   - and the two hold at least one value token IN COMMON (sharesValue): the
//     identifier the claim is about (go.mod, Redis, TTL, SESSION_STORE). Two
//     parallel notes in one template about two different things ("auth-service
//     runs on node-3" / "billing-service runs on node-7") share every word but
//     the names, so they clear the similarity bar, and a raised bar cannot help:
//     such a template scores HIGHER than a real replacement, which rewords more.
//     A replacement keeps the subject and changes the value; parallel facts
//     change the subject. The save path does not need this test, because a
//     refused fold costs nothing there, while here a false pair reorders a block
//     and labels a row. A replacement whose only value is the one that changed
//     ("listens on port 8080" / "on port 9090") is missed, which is the safe
//     direction: the rows stay as they were.
//   - chronology by created_at, the stamp a fold never moves (updated_at moves
//     on every strengthening and access). The stamps must differ and both must
//     be readable. A tie, or an unreadable stamp, makes no pair, which also
//     keeps the relation acyclic for the assembler that orders the pairs.
//
// A row is no party to a pair when it is resolved (its `[resolved]` marker
// already says it is retired), when its validity window is closed or not yet
// open at now (stage 2 withholds it), or when the two rows are joined by a live
// `supersedes` or `contradicts` edge in either direction: the explicit
// relation's own handling (the supersede demotion, stage 5's separation) wins.
// Other relations do not count, because the linking worker writes cosine
// `related` edges between exactly these rows, and excluding on those would
// switch the rule off on every install with embeddings.
//
// It is a LEXICAL GUESS and is rendered as one. It never withholds a row: the
// older row stays and says it may have been replaced.
//
// The result maps an older row's id to its replacements' ids, newest first (ties
// by id). It is nil when no pair is found.
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
			if a.c.Category == b.c.Category {
				if mergeScore(a.tokens, b.tokens) <= 0 {
					continue
				}
			} else if jaccard(a.tokens, b.tokens) < upsertCrossCategoryThreshold {
				continue
			}
			if !sharesValue(a.values, b.values) || !lacksValue(a.values, b.values) || !lacksValue(b.values, a.values) {
				continue
			}
			if subjectSwapped(a.fields, b.fields) {
				continue
			}
			older, newer := a, b
			if b.created.Before(a.created) {
				older, newer = b, a
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

// subjectSwapped reports whether two texts differ, somewhere, by ONE plain word
// standing in the other's place: "auth-service" for "billing-service", "staging"
// for "reporting", `make test` for `make lint`. The two word sequences are
// aligned on their longest common subsequence, and each run of words between two
// aligned ones is a change. A change of exactly one word on each side, where
// neither word is value-SHAPED (no digit, path or identifier mark, inner capital
// or name) and both are content words (at least four letters, not on the short
// list of function words below), names a different subject in the same
// sentence frame. That is two parallel facts, both live, not a replacement.
//
// A replacement changes values in that frame (a version, a path, a name) and
// rewords around them, and a rewording is an insertion ("pins" → "now pins") or
// a run of several words ("the code imports" → "every import moved to"), never a
// single plain word swapped in place. A word inside a code span counts by its
// shape, so `make test` → `make lint` is a swapped step, not a new value: it can
// be a change of procedure or a second step beside the first, the text cannot
// say which, and an unmarked pair is the safe reading.
func subjectSwapped(a, b []valueField) bool {
	// Only the CHANGED region is aligned. The words both texts open and close
	// with are common to them by definition, and they are most of a long note
	// that differs in one place; aligning them made the table quadratic in the
	// whole note for nothing.
	for len(a) > 0 && len(b) > 0 && a[0].word == b[0].word {
		a, b = a[1:], b[1:]
	}
	for len(a) > 0 && len(b) > 0 && a[len(a)-1].word == b[len(b)-1].word {
		a, b = a[:len(a)-1], b[:len(b)-1]
	}
	// A changed region longer than maxAlignedWords on either side is refused
	// without being aligned: no pair, so no marker and no move, which is the
	// safe direction for a guess (see maxAlignedWords).
	if len(a) > maxAlignedWords || len(b) > maxAlignedWords {
		return true
	}
	n, m := len(a), len(b)
	// lcs[i][j] is the common-subsequence length of a[i:] and b[j:].
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
	si, sj := 0, 0 // start of the current change on each side
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

// maxAlignedWords bounds the changed region subjectSwapped aligns, so the
// alignment costs at most a 65x65 table per pair on the session-start path
// whatever the notes' length. 64 words is nearly three times the largest changed
// region of the replacements this rule is measured against: 23 words (the
// storyline's renamed endpoint), 17 (the version upgrade), 16 (the reversed
// decision), and at most 12 across the bench maintenance corpus's eleven pairs.
// A replacement rewords the clause around its values, not a page. Two notes whose changed
// region is longer than that differ in more than a value, and a guess about
// them is not worth its cost or its risk.
const maxAlignedWords = 64

// subjectWord reports whether a word is a plain content word: not value-shaped,
// at least four letters, and not a function word. Short and function words are
// grammar ("is" → "was", "then" → "also"), and swapping one is a rewording.
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

// functionWords are the four-letter-and-longer words that carry grammar rather
// than a subject, so a swap of one is rewording.
var functionWords = map[string]bool{
	"also": true, "been": true, "does": true, "from": true, "have": true, "into": true,
	"just": true, "longer": true, "more": true, "much": true, "must": true, "never": true,
	"only": true, "once": true, "should": true, "since": true, "still": true, "than": true,
	"that": true, "then": true, "there": true, "these": true, "they": true, "this": true,
	"those": true, "under": true, "until": true, "were": true, "what": true, "when": true,
	"where": true, "which": true, "while": true, "will": true, "with": true, "would": true,
	"always": true, "after": true, "before": true, "every": true,
}

// sharesValue reports whether two value-token sets hold a token in common.
func sharesValue(a, b map[string]bool) bool {
	for t := range a {
		if b[t] {
			return true
		}
	}
	return false
}

// scopesEqual reports whether two scope maps hold the same keys with the same
// values. A nil map and an empty one are the same (unscoped) scope.
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

// replacementPairsWithin reports the lexical replacement pairs among rows, keyed
// both ways round, by the rule lexicalReplacements states and through it, so the
// near-duplicate removal and the marker can never disagree about what a pair is.
// The live `supersedes` and `contradicts` edges among the rows are read here so
// an explicit relation still wins exactly as it does for the marker.
//
// It exists for the near-duplicate REMOVAL, which runs before the read that
// names the pairs: a removal that took one row of a pair out first would leave
// nothing for the marker to name, and the reader would get one of the two claims
// with nothing saying the other exists.
func replacementPairsWithin(ctx context.Context, db Queryer, rows []Memory, now time.Time) (map[[2]string]bool, error) {
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
	repl := lexicalReplacements(cands, edges, now)
	if len(repl) == 0 {
		return nil, nil
	}
	out := make(map[[2]string]bool, 2*len(repl))
	for older, newer := range repl {
		for _, n := range newer {
			out[[2]string{older, n}] = true
			out[[2]string{n, older}] = true
		}
	}
	return out, nil
}

// explicitEdgesWithin is the live `supersedes` and `contradicts` edges whose
// both endpoints are in ids: the two relations that take a pair out of
// lexicalReplacements.
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

// withoutReplacementPairs drops the near-duplicate verdicts whose two rows are a
// lexical replacement pair. A replacement is not a duplicate (the save path
// refuses to fold it for that reason), and the lexical guess never withholds a
// row: both rows stay, the newer one is shown ahead of the older and the older
// one is marked. Keeping both, rather than removing the older row, is the
// choice for that second reason: the guess can be wrong, and a wrong guess that
// removed a row would hide a live claim with nothing on the page saying so.
func withoutReplacementPairs(pairs []demotionPairs, repl map[[2]string]bool) []demotionPairs {
	if len(repl) == 0 {
		return pairs
	}
	kept := make([]demotionPairs, 0, len(pairs))
	for _, pr := range pairs {
		if repl[[2]string{pr.loser, pr.winner}] {
			continue
		}
		kept = append(kept, pr)
	}
	return kept
}
