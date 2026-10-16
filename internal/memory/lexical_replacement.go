package memory

import (
	"sort"
	"time"
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
//     keeps the relation acyclic for arrangeLexicalReplacements.
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
		parties = append(parties, party{c: c, created: created, tokens: tokens, values: valueTokens(c.Content)})
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

// arrangeLexicalReplacements orders each row's replacements (lexicalReplacements)
// ahead of it, for a passive read whose order is importance times decay rather
// than relevance. A replacement that sits BEHIND its older row is moved up to
// directly ahead of it, and the rows in between shift down by one. Moving the
// newer row up, rather than sinking the older one, is what keeps the older row
// inside a slice cap whenever it was: it is marked, not withheld.
//
// A pinned row never moves, and no row is moved ahead of one. A pin is a slot
// guarantee, and a guess is not an explicit `supersedes` edge, which is the only
// relation the passive order lets reorder a pin (passiveArrange, with the
// replacement reserved a slot). A lexical guess gets the treatment a newer
// `contradicts` partner of a pinned row gets: the row is marked and keeps its
// place.
//
// A chain (v4, v5, v6) is arranged newest first: placing a row first places its
// replacements, each of which places its own, and created_at strictly increases
// along every step, so the recursion ends. Rows are otherwise kept in their
// input order. The input is not modified.
func arrangeLexicalReplacements(rows []Candidate, repl map[string][]string) []Candidate {
	if len(repl) == 0 || len(rows) < 2 {
		return rows
	}
	at := make(map[string]int, len(rows))
	for i := range rows {
		at[rows[i].ID] = i
	}
	out := make([]Candidate, 0, len(rows))
	placed := make([]bool, len(rows))
	var place func(i int)
	place = func(i int) {
		if placed[i] {
			return
		}
		if !rows[i].Pinned {
			for _, id := range repl[rows[i].ID] {
				j, ok := at[id]
				if !ok || j <= i || placed[j] || rows[j].Pinned {
					continue
				}
				place(j)
			}
		}
		placed[i] = true
		out = append(out, rows[i])
	}
	for i := range rows {
		place(i)
	}
	return out
}
