package memory

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode"
)

// AsOfUnknownNote is the sentence a surface shows when a historical read could
// not place n in-scope memories at the requested instant, and "" for none.
func AsOfUnknownNote(n int) string {
	if n <= 0 {
		return ""
	}
	const because = " — the history table (schema v17) backfilled nothing, so no version of %s exists at or before the " +
		"requested instant and Ghost cannot say what %s held then."
	if n == 1 {
		return "as_of: 1 memory in scope is unknown before its first recorded version" +
			fmt.Sprintf(because, "it", "it") +
			" It is left out of the answer rather than answered with today's text, and nothing here is a guess about it."
	}
	return fmt.Sprintf("as_of: %d memories in scope are unknown before their first recorded version", n) +
		fmt.Sprintf(because, "them", "they") +
		" They are left out of the answer rather than answered with today's text, and nothing here is a guess about them."
}

// asOfInstant is the clock one retrieval runs against: AsOf when the request is
// historical, Now otherwise. One function because the two must never be read
// separately — a row chosen by one clock and ranked by another would be a
// ranking of a set the retrieval did not return.
func asOfInstant(req CandidateRequest) time.Time {
	if req.AsOf != nil {
		return *req.AsOf
	}
	return req.Now
}

// AsOfSourceNote is the sentence stating where a historical answer's rows come
// from, and it is the same sentence on every surface that shows one.
func AsOfSourceNote(t time.Time) string {
	return "as_of " + t.UTC().Format(time.RFC3339) +
		": a historical read — these are the versions memory_history recorded at or before that instant, " +
		"including memories deleted since and excluding memories that did not exist yet."
}

// AsOfUnversionedNote is the sentence stating which halves of a block a
// historical read cannot fill, and it is the same sentence on every surface that
// renders one.
func AsOfUnversionedNote() string {
	return "Tasks, decisions and learned context are not versioned, so they are omitted from a historical read rather " +
		"than shown as they are now; a memory row's tags, scope, pin, confidence, agent, source_ref and validity " +
		"window are not versioned either, and where shown they are the current row's. Apart from those, nothing in " +
		"this block is a claim about the present."
}

func asOfBorrowedWindowSentence() string {
	return "The window's bounds, like the row's tags, scope, pin, confidence, agent and source_ref, are read from " +
		"the current row, because the history table does not version them."
}

// AsOfValidityNote is the sentence an as_of LISTING prints: it judged validity
// at the requested instant, and withheld (and counts) what that excluded. The
// two surfaces that render one are ghost_project_context's as_of branch and the
// ghost context --as-of session block, both of which judge through
// memory.ValidityAt. It leads with the borrow and only then states the verdict,
// so a reader meets the caveat before the claim it qualifies.
func AsOfValidityNote(at time.Time, withheld int) string {
	note := asOfBorrowedWindowSentence() + " Validity judged at " + at.UTC().Format(time.RFC3339) +
		": a memory whose validity window had closed or had not yet opened at that instant is withheld."
	switch {
	case withheld == 1:
		note += " 1 memory was withheld for that reason, so it is out of window at that instant rather than absent from the store."
	case withheld > 1:
		note += fmt.Sprintf(" %d memories were withheld for that reason, so they are out of window at that instant rather than absent from the store.", withheld)
	}
	return note
}

// AsOfBorrowedWindowNote is the sentence a read that judged no window prints,
// which today is ghost_memory_search with as_of. It leads with the same borrow
// sentence the listings lead with, and then says what the absence of a verdict
// means rather than leaving the reader to infer one from a missing marker.
func AsOfBorrowedWindowNote(at time.Time) string {
	return asOfBorrowedWindowSentence() + " No verdict was drawn from them for this read, so no row is withheld as " +
		"expired or labelled not yet valid: a bound the row holds today is not a claim about " +
		at.UTC().Format(time.RFC3339) + ", and a missing marker is not evidence a row was inside its window then."
}

type asOfLeg struct {
	tokens []string
	prefix bool
}

func historicalTerms(query string, logger *slog.Logger) []asOfLeg {
	raw := ftsQueryTerms(query, ftsSearchWordLimit, logger)
	if len(raw) == 0 {
		return nil
	}
	terms := make([]asOfLeg, 0, len(raw))
	for _, t := range raw {
		tokens := tokenizeForMatch(t.clean)
		if len(tokens) == 0 {
			continue
		}
		terms = append(terms, asOfLeg{tokens: tokens, prefix: t.prefix})
	}
	return terms
}

// matches reports whether the term occurs in the token list. A phrase term
// matches on adjacency, which is what FTS5 does with a quoted term; a prefix
// term matches on the last token's prefix, which is what the trailing `*` does.
// The whole term must match — never a prefix of the first token — so a query for
// "wal" cannot be answered by "walrus".
func (l asOfLeg) matches(tokens []string) bool {
	if len(l.tokens) == 0 || len(tokens) < len(l.tokens) {
		return false
	}
	for start := 0; start+len(l.tokens) <= len(tokens); start++ {
		window := tokens[start : start+len(l.tokens)]
		ok := true
		for i, want := range l.tokens {
			got := window[i]
			if i == len(l.tokens)-1 && l.prefix {
				if !strings.HasPrefix(got, want) {
					ok = false
					break
				}
				continue
			}
			if got != want {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// tokenizeForMatch splits text into the tokens a term can match: runs of letters
// and digits, case-folded. FTS5's default tokenizer does the same and also
// removes diacritics; this does not, so an accented word matches only when the
// query spells it the same way. That is a narrower match on a path that is
// already narrower than the FTS leg, and it is stated rather than approximated.
func tokenizeForMatch(text string) []string {
	var (
		out  []string
		cur  strings.Builder
		emit = func() {
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		}
	)
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur.WriteRune(unicode.ToLower(r))
			continue
		}
		emit()
	}
	emit()
	return out
}

type asOfHit struct {
	row  AsOfRow
	hits int
	rank int
}

func (cand *Store) candidatesAsOf(ctx context.Context, req CandidateRequest, p SearchParams, ftsTopK int, set *CandidateSet) (*CandidateSet, error) {
	at := asOfInstant(req)
	asof, err := ReadMemoriesAsOf(ctx, cand.queryDB(), req.Mode, req.ProjectID, at)
	if err != nil {
		return nil, err
	}
	set.Unrecorded = len(asof.Unknown)

	terms := historicalTerms(req.Query, cand.logger)
	var hits []asOfHit
	// No extracted term means no term matched, never "no keyword filter": the
	if len(terms) > 0 {
		for _, row := range asof.Rows {
			n := 0
			tokens := tokenizeForMatch(row.Content)
			for _, term := range terms {
				if term.matches(tokens) {
					n++
				}
			}
			if n == 0 {
				continue
			}
			hits = append(hits, asOfHit{row: row, hits: n})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].hits != hits[j].hits {
			return hits[i].hits > hits[j].hits
		}
		si := asOfComposite(hits[i].row, at)
		sj := asOfComposite(hits[j].row, at)
		if si != sj {
			return si > sj
		}
		return hits[i].row.ID < hits[j].row.ID
	})

	ftsStatus := LegStatus{
		Applicable: true, Attempted: true, Available: true,
		Eligible:         len(hits),
		Truncated:        len(hits) >= ftsTopK,
		CoverageComplete: len(hits) < ftsTopK,
	}
	set.Legs["fts"] = ftsStatus
	set.Legs["vector"] = LegStatus{Applicable: false}
	set.EdgesStatus = EdgeStatus{Status: edgesNotApplicable}

	if len(hits) == 0 {
		return set, nil
	}

	scores := make(map[string]float64, len(hits))
	for i := range hits {
		hits[i].rank = i
		scores[hits[i].row.ID] = 1.0 / float64(p.RRFK+i+1) *
			statusDemotionFactor(hits[i].row.ResolvedAt != nil, hits[i].row.ProjectID, p.ProjectID)
	}

	ordered := asOfScoredOrder(hits, scores)
	windowRows := make([]Memory, 0, min(len(ordered), req.Fetch.Limit))
	for i, h := range ordered {
		if i >= req.Fetch.Limit {
			break
		}
		windowRows = append(windowRows, h.row.Memory)
	}
	selected := decayRank(windowRows, scores, p, req.Fetch.Limit, at)
	selected = asOfSupersedeDemote(selected, hits)

	tail := asOfTail(ordered, selected, req.Fetch.Limit, scores, at)

	rows := make([]Candidate, 0, len(selected)+len(tail))
	for _, m := range append(selected, tail...) {
		rows = append(rows, asOfCandidate(m, hits, scores, at))
	}
	set.Rows = rows
	set.Widened = len(rows) > req.Fetch.Limit
	return set, nil
}

func asOfComposite(row AsOfRow, at time.Time) float64 {
	return float64(row.Importance) * DecayFactor(row.Category, row.Content, RetentionProject, row.Pinned, ageDays(row.CreatedAt, at))
}

// asOfSupersedeDemote moves a memory that a live `supersedes` claim named at T
// below the memory that replaced it, when both are in the answer.
func asOfSupersedeDemote(window []Memory, hits []asOfHit) []Memory {
	if len(window) < 2 {
		return window
	}
	claimedBy := make(map[string]string, len(hits))
	for _, h := range hits {
		if h.row.SupersededBy != "" {
			claimedBy[h.row.ID] = h.row.SupersededBy
		}
	}
	present := make(map[string]bool, len(window))
	for _, m := range window {
		present[m.ID] = true
	}
	penalty := map[string]int{}
	for _, m := range window {
		if superseder, ok := claimedBy[m.ID]; ok && present[superseder] {
			penalty[m.ID] = 1
		}
	}
	if len(penalty) == 0 {
		return window
	}
	return StableDemote(window, func(m Memory) string { return m.ID }, penalty)
}

func asOfScoredOrder(hits []asOfHit, scores map[string]float64) []asOfHit {
	ordered := make([]asOfHit, len(hits))
	copy(ordered, hits)
	sort.SliceStable(ordered, func(i, j int) bool {
		si := scores[ordered[i].row.ID]
		sj := scores[ordered[j].row.ID]
		if si != sj {
			return si > sj
		}
		return ordered[i].row.ID < ordered[j].row.ID
	})
	return ordered
}

// asOfTail is the rows the window cut, in the same order the window came back
// in, and BOUNDED at the window like hydrateTail's. It exists as a function
// rather than a sort.SliceStable at the call site because the current path's tail
// and this one must agree on both the order and the bound: a predicate that
// reaches past the window is backfilled from it, so two orderings would make the
// same predicate reach for different rows, and an unbounded tail would walk the
// whole matching corpus — one hydrated Memory per match — through every
// assembler stage and the trace's signals, which is the work the current path caps
// at roughly twice the window.
func asOfTail(ordered []asOfHit, window []Memory, limit int, scores map[string]float64, at time.Time) []Memory {
	taken := make(map[string]bool, len(window))
	for _, m := range window {
		taken[m.ID] = true
	}
	tail := make([]Memory, 0, min(len(ordered), limit))
	for _, h := range ordered {
		if len(tail) == limit {
			break
		}
		if !taken[h.row.ID] {
			taken[h.row.ID] = true
			tail = append(tail, h.row.Memory)
		}
	}
	sort.SliceStable(tail, func(i, j int) bool {
		si := scores[tail[i].ID] * DecayFactor(tail[i].Category, tail[i].Content, RetentionProject, tail[i].Pinned, ageDays(tail[i].CreatedAt, at))
		sj := scores[tail[j].ID] * DecayFactor(tail[j].Category, tail[j].Content, RetentionProject, tail[j].Pinned, ageDays(tail[j].CreatedAt, at))
		if si != sj {
			return si > sj
		}
		return tail[i].ID < tail[j].ID
	})
	return tail
}

func asOfCandidate(m Memory, hits []asOfHit, scores map[string]float64, at time.Time) Candidate {
	c := Candidate{Memory: m}
	c.Base = scores[m.ID]
	c.AgeDays = ageDays(m.CreatedAt, at)
	c.Decay = DecayFactor(m.Category, m.Content, RetentionProject, m.Pinned, c.AgeDays)
	c.Score = c.Base * c.Decay
	c.FTSRank, c.VectorRank, c.VectorScore = -1, -1, -1
	for _, h := range hits {
		if h.row.ID == m.ID {
			c.FTSRank = h.rank
			break
		}
	}
	return c
}
