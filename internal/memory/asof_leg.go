package memory

// The historical retrieval leg (issue #647). memory_history is a VERSION log, so
// a read at an instant T is a selection rather than a reconstruction: take each
// memory's newest row at or before T and that row IS its state at T. This file
// turns that selection into a ranked candidate set with the same shape the
// assembler consumes for a current read, which is what lets one pipeline serve
// both.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
)

// AsOfUnknownNote is the sentence a surface shows when a historical read could
// not place n in-scope memories at the requested instant, and "" for none.
//
// It is a function of the count rather than of a set, because the assembler has
// only the count (CandidateSet.Unrecorded) while a direct caller has the set —
// and the two must not be able to word the same fact differently. The set's own
// UnknownNote delegates here for the same reason.
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
//
// It lives here rather than in a surface because the store owns the fact and two
// surfaces already need it: a search, which adds what its retrieval could and
// could not do, and a project listing, which only reads a set. A surface that
// worded it itself would let the two drift, and a drift here is not cosmetic —
// it is the difference between a reader knowing the block is a past reading and
// taking it for a present one.
func AsOfSourceNote(t time.Time) string {
	return "as_of " + t.UTC().Format(time.RFC3339) +
		": a historical read — these are the versions memory_history recorded at or before that instant, " +
		"including memories deleted since and excluding memories that did not exist yet."
}

// AsOfUnversionedNote is the sentence stating which halves of a block a
// historical read cannot fill, and it is the same sentence on every surface that
// renders one.
//
// It is its own function because the omission is a separate fact from the
// provenance of the rows, and both surfaces that read a past set have to say it:
// a block that disclosed its instant and then printed today's tasks under it
// would be contradicted by its own second half.
//
// It carves out every field a historical row line DOES carry from the current
// row — tags, scope, pin, confidence, agent, source_ref and the validity window,
// which are exactly the fields a row line renders that the history table does
// not version — because "nothing in this block is a claim about the present"
// would be false about them. AsOfValidityNote is where the window's borrow is
// stated beside the rows; this sentence names the whole set too, so the two
// notes cannot read as a contradiction.
func AsOfUnversionedNote() string {
	return "Tasks, decisions and learned context are not versioned, so they are omitted from a historical read rather " +
		"than shown as they are now; a memory row's tags, scope, pin, confidence, agent, source_ref and validity " +
		"window are not versioned either, and where shown they are the current row's. Apart from those, nothing in " +
		"this block is a claim about the present."
}

// AsOfValidityNote is the sentence stating that the validity window a historical
// read renders — like the row's other unversioned fields — comes from the CURRENT
// row rather than the version, and it is the same sentence on every surface that
// renders one.
//
// It lives here, beside the other two historical notes, because the store owns
// the fact: memory_history versions a memory's STATE in content, category,
// importance, resolved_at, source and project_id (the six columns AsOfRow's doc
// names), and the rest of a row line — tags, scope, pin, confidence, agent,
// source_ref and the validity triple — is not among them, so the read takes each
// from the live row. A surface that worded the borrow itself would let the two
// drift, and a drift here is not cosmetic — it is the difference between a
// reader taking a present-day window for the window that held at the instant the
// block is a reading of, and knowing it is not.
//
// The note names the full borrowed set rather than the window alone, and it does
// NOT say "the rest of the line is the version's": that would be false, because
// the same row line prints the current row's tags, confidence and provenance
// beside the window. The instant is named because the note's whole point is what
// it is NOT a claim about; the block's AsOfSourceNote names the same instant, and
// a reader who meets either line first has met it. The note says in as many words
// that no verdict is drawn from the window, because a verdict at the instant
// would be exactly the claim the borrow makes false.
func AsOfValidityNote(at time.Time) string {
	return "A memory row's validity window, and its tags, scope, pin, confidence, agent and source_ref, are read " +
		"from the current row rather than the version the row's content and category come from. The window shown " +
		"is the CURRENT one, not the window the memory held at " + at.UTC().Format(time.RFC3339) + ", so no expiry " +
		"or future-start verdict is drawn from it."
}

// AsOfValidityState is the validity state a HISTORICAL listing may draw from a
// row's borrowed window, and it is what the as_of surfaces pass to ValidityLabel.
//
// It is ValidityState's answer with its clock-dependent half taken out. expired
// and future measure a clock against the window, and the window is the current
// row's rather than the version the line comes from (AsOfRow's doc says so, and
// AsOfValidityNote is where a surface states the borrow), so neither is drawn.
// What is left is the clock-independent fact about the row: unset when it states
// no validity at all, unverified when it states a window nobody ever verified,
// and valid when the window carries a readable verification stamp (ValidityLabel
// prints the stamp itself and renders no word for valid). A window that is
// currently closed or not yet open therefore still reads unverified when no
// verification was recorded — that the claim was never checked is a fact about
// the row the window came from, not about when it is read — and reads with no
// marker when it was verified.
//
// now is the current clock, named because ValidityState takes one; the answer
// does not depend on it, which is the point of dropping the states that do.
func AsOfValidityState(validFrom, validUntil, verifiedAt *string, now time.Time) string {
	state, _ := ValidityState(validFrom, validUntil, verifiedAt, now)
	switch state {
	case ValidityExpired, ValidityFuture:
		// The verdict is dropped; the verification fact is not. A readable
		// window with no readable verification stamp is unverified whatever the
		// clock says about the window, and a verified window needs no marker
		// here because ValidityLabel prints the stamp itself.
		if _, ok := parseStoredStamp(verifiedAt); !ok {
			return ValidityUnverified
		}
		return ""
	default:
		return state
	}
}

// asOfLeg is a query term prepared for matching against text, rather than for
// FTS5.
//
// The two are not the same shape. FTS5 takes a query string and indexes the
// tokens itself; here the tokens have to be matched by hand against a version's
// content, because the index holds CURRENT content and the read is about a past
// one. A term therefore keeps its token list — FTS5's unicode61 tokenizer
// splits on non-alphanumerics, so `ghost_windows_arm64` is a PHRASE of three
// tokens and matches only where they are adjacent — and its prefix flag, so the
// `sqlite*` syntax the tool description advertises means the same thing here as
// it does in the FTS leg.
type asOfLeg struct {
	tokens []string
	prefix bool
}

// historicalTerms tokenizes a query the way the FTS leg's sanitizer selects it
// (ftsQueryTerms, so the term budget, the identifier ranking and the stopword
// rule are one rule) and prepares each term for matching.
func historicalTerms(query string) []asOfLeg {
	raw := ftsQueryTerms(query, ftsSearchWordLimit)
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

// asOfHit is one matched version and the number of query terms it matched.
type asOfHit struct {
	row  AsOfRow
	hits int
	// rank is the hit's place in the relevance order — matched terms first, then
	// the historical composite — and is what the trace reports as the keyword
	// leg's rank. It is not a bm25 rank: nothing here is scored by FTS5, because
	// nothing here is indexed by it.
	rank int
}

// candidatesAsOf is the whole historical retrieval: one read of the recorded
// set, one keyword pass over the versions that matched, the same window and
// decay the current path applies, and the demotion the recorded supersede
// sequence supports.
//
// What it deliberately does NOT do is borrow anything that only describes the
// present. No vector leg (an embedding is a vector of the text as it is now),
// no link read (memory_links has no history), no near-duplicate demotion (same
// reason), no scope narrowing against the live pool (the pool IS the historical
// set). Each of those is a way to answer a question about T with a fact about
// now, and the caller is told which of them did not run.
func (cand *Store) candidatesAsOf(ctx context.Context, req CandidateRequest, p SearchParams, ftsTopK int, set *CandidateSet) (*CandidateSet, error) {
	at := asOfInstant(req)
	asof, err := ReadMemoriesAsOf(ctx, cand.queryDB(), req.Mode, req.ProjectID, at)
	if err != nil {
		return nil, err
	}
	set.Unrecorded = len(asof.Unknown)

	terms := historicalTerms(req.Query)
	var hits []asOfHit
	for _, row := range asof.Rows {
		n := 0
		if len(terms) > 0 {
			tokens := tokenizeForMatch(row.Content)
			for _, term := range terms {
				if term.matches(tokens) {
					n++
				}
			}
			if n == 0 {
				continue
			}
		}
		hits = append(hits, asOfHit{row: row, hits: n})
	}
	// Relevance order: more matched terms first, then the historical composite
	// (the same decay rule the current read ranks on, at T), then id. The
	// composite is the tie-break rather than the primary key because a term count
	// is the only relevance signal a keyword pass over unindexed text has — and
	// saying so is why this order cannot be compared with an FTS5 ranking.
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

	// The keyword leg's status is reported against the leg that ran. Truncated
	// carries its production meaning: the leg returned at least as many rows as
	// it was asked for, so it cannot support a completeness claim.
	ftsStatus := LegStatus{
		Applicable: true, Attempted: true, Available: true,
		Eligible:         len(hits),
		Truncated:        len(hits) >= ftsTopK,
		CoverageComplete: len(hits) < ftsTopK,
	}
	set.Legs["fts"] = ftsStatus
	// Not applicable, not attempted, and not a zero: no vector describes a past
	// wording, so there was nothing to attempt and nothing failed.
	set.Legs["vector"] = LegStatus{Applicable: false}
	// No conflict or duplicate claim in either direction, because no link graph
	// was read. The stage that would render "no link joins two of these
	// candidates" says nothing for a status that is neither ok nor unavailable.
	set.EdgesStatus = EdgeStatus{Status: edgesNotApplicable}

	if len(hits) == 0 {
		return set, nil
	}

	scores := make(map[string]float64, len(hits))
	for i := range hits {
		hits[i].rank = i
		// The keyword-only base the FTS paths use (see decayRank): 1/(K+rank+1).
		// Fusion has no second leg to fuse, so the rank-based base is the whole
		// score rather than one contribution to a sum.
		scores[hits[i].row.ID] = 1.0 / float64(p.RRFK+i+1)
	}

	// The window and the tail, cut exactly as the current path cuts them: the
	// window is the top Fetch.Limit by base, decay reorders what is left of it,
	// and the tail is the rest in the same decay order so a predicate that
	// removes a window row is backfilled by the row that would have ranked next.
	windowRows := make([]Memory, 0, min(len(hits), req.Fetch.Limit))
	for i, h := range hits {
		if i >= req.Fetch.Limit {
			break
		}
		windowRows = append(windowRows, h.row.Memory)
	}
	selected := decayRank(windowRows, scores, p, req.Fetch.Limit, at)
	selected = asOfSupersedeDemote(selected, hits)

	tail := asOfTail(hits, req.Fetch.Limit, scores, at)

	rows := make([]Candidate, 0, len(selected)+len(tail))
	for _, m := range append(selected, tail...) {
		rows = append(rows, asOfCandidate(m, hits, scores, at))
	}
	set.Rows = rows
	set.Widened = len(rows) > req.Fetch.Limit
	return set, nil
}

// asOfComposite is the historical composite the current read ranks on: the
// version's importance times the category-aware decay of its age measured to T.
// It is DecayFactor over the version's own category and importance, which is why
// a relabelled memory decays as what it was rather than as what it became -- and
// over the `project` tier, because the change log records no tier at all (see
// sortAsOfRows for why reading today's tier into a past ranking would be a claim
// the table does not support).
func asOfComposite(row AsOfRow, at time.Time) float64 {
	return float64(row.Importance) * DecayFactor(row.Category, RetentionProject, row.Pinned, ageDays(row.CreatedAt, at))
}

// asOfSupersedeDemote moves a memory that a live `supersedes` claim named at T
// below the memory that replaced it, when both are in the answer.
//
// The same co-presence rule the current path applies: a claim about a memory
// that is not in the answer cannot reorder anything, and demoting on it anyway
// would make a historical read disagree with itself. The claim itself comes from
// the recorded supersede/unsupersede sequence (AsOfRow.SupersededBy), never from
// the live link table, whose edges describe the present.
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

// asOfTail is the rows the window cut, in the same order the window came back
// in, and BOUNDED at the window like hydrateTail's. It exists as a function
// rather than a sort.SliceStable at the call site because the current path's tail
// and this one must agree on both the order and the bound: a predicate that
// reaches past the window is backfilled from it, so two orderings would make the
// same predicate reach for different rows, and an unbounded tail would walk the
// whole matching corpus — one hydrated Memory per match — through every
// assembler stage and the trace's signals, which is the work the current path caps
// at roughly twice the window.
//
// The bound is disclosed rather than silent: the keyword leg's Truncated status
// says the leg was cut short, and a caller that trusts CoverageComplete instead
// learns nothing from this function either way.
func asOfTail(hits []asOfHit, limit int, scores map[string]float64, at time.Time) []Memory {
	taken := make(map[string]bool, limit)
	for i := range hits {
		if i < limit {
			taken[hits[i].row.ID] = true
		}
	}
	tail := make([]Memory, 0, min(len(hits), limit))
	for _, h := range hits {
		if len(tail) == limit {
			break
		}
		if !taken[h.row.ID] {
			taken[h.row.ID] = true
			tail = append(tail, h.row.Memory)
		}
	}
	sort.SliceStable(tail, func(i, j int) bool {
		si := scores[tail[i].ID] * DecayFactor(tail[i].Category, RetentionProject, tail[i].Pinned, ageDays(tail[i].CreatedAt, at))
		sj := scores[tail[j].ID] * DecayFactor(tail[j].Category, RetentionProject, tail[j].Pinned, ageDays(tail[j].CreatedAt, at))
		if si != sj {
			return si > sj
		}
		return tail[i].ID < tail[j].ID
	})
	return tail
}

// asOfCandidate pairs a version with the facts the historical leg produced for
// it. VectorRank and VectorScore are -1 — the marks for a leg that did not
// retrieve the row — so a consumer reading the trace cannot mistake the absent
// vector leg for a cosine of zero, which would be a real measurement of "no
// similarity".
func asOfCandidate(m Memory, hits []asOfHit, scores map[string]float64, at time.Time) Candidate {
	c := Candidate{Memory: m}
	c.Base = scores[m.ID]
	c.AgeDays = ageDays(m.CreatedAt, at)
	c.Decay = DecayFactor(m.Category, RetentionProject, m.Pinned, c.AgeDays)
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
