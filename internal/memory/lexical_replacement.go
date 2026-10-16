package memory

import (
	"sort"
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
