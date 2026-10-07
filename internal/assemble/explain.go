package assemble

import (
	"fmt"
	"strings"

	"github.com/wcatz/ghost/internal/memory"
)

// explainProjection is the ghost_memory_search explain payload, built from the
// run that produced the answer: the same candidate set, the same stages, the
// same budget, and the items the answer finally holds. It is a READING of the
// run, not a second one — nothing here retrieves, ranks, filters or scores, and
// every number it reports was either stamped by the stage that computed it
// (memory.RankFact, Signals, Decision) or read off the memory itself.
//
// WHICH ROWS are in the payload is the candidate set's whole account of what the
// ranking saw: the rows it returned (Rows, including the window's tail), the
// pool candidates it did not return (Excluded: scope narrowing and the rows
// beyond the tail), and the candidates the vector floor removed outright
// (FloorDropped). WHICH are marked included is the final item list and nothing
// else, so a row the validity stage, the category or retention filter, the item
// budget or the response-fit pass withheld is excluded here with that reason,
// and the included set equals the set the answer renders.
//
// EVERY STORED STRING goes through the renderers the formatted answer uses:
// content through Data (over its 120-rune snippet), and ids, project names,
// scope keys and values, tags-free enums and leg error text through Token or
// Data. The payload is JSON, which escapes a newline and a quote, but it does
// not escape « or », and an agent reads the string, not the bytes.
func (p *pipeline) explainProjection(res Result) *memory.SearchExplain {
	req := p.req
	ex := &memory.SearchExplain{
		ProjectID:       Token(req.ProjectID),
		Query:           req.Query,
		Limit:           p.trace.Limit,
		VectorAvailable: p.trace.VectorAvailable,
		Rows:            []memory.ExplainRow{},
	}
	if len(req.Scope) > 0 {
		ex.Scope = make(map[string]string, len(req.Scope))
		for k, v := range req.Scope {
			ex.Scope[Token(k)] = Token(v)
		}
	}

	// Compare once: which ids the answer holds, and at what 1-based rank.
	rank := make(map[string]int, len(res.Items))
	for i, it := range res.Items {
		rank[it.ID] = i + 1
	}

	// The universe of memories, deduplicated, in the order a reader should meet
	// them: the answer first in its own order, then everything else the ranking
	// examined in the order it examined it.
	byID := map[string]memory.Memory{}
	var order []string
	seen := map[string]bool{}
	add := func(m memory.Memory) {
		if !seen[m.ID] {
			seen[m.ID] = true
			byID[m.ID] = m
			order = append(order, m.ID)
		}
	}
	rowByID := make(map[string]memory.Candidate, len(p.set.Rows))
	for _, c := range p.set.Rows {
		rowByID[c.ID] = c
	}
	for _, it := range res.Items {
		if c, ok := rowByID[it.ID]; ok {
			add(c.Memory)
		}
	}
	for _, c := range p.set.Rows {
		add(c.Memory)
	}
	// The retriever's removed near-duplicate losers: not in Rows, but the stage 6
	// trace holds each, so they are reported as candidates that were not included
	// rather than absent.
	for _, l := range p.set.DroppedLosers {
		if _, ok := p.losers[l.ID]; ok {
			add(l.Memory)
			rowByID[l.ID] = l.Candidate
		}
	}
	for _, m := range p.set.Excluded {
		add(m)
	}
	for _, m := range p.set.FloorDropped {
		add(m)
	}

	scopeNames, scopeTotal := memory.ClampScopeKeys(scopeKeys(req.Scope))
	for i, k := range scopeNames {
		scopeNames[i] = Token(k)
	}

	factsKnown := len(p.set.RankFacts) > 0
	statusDemoted, tierDecay := false, false
	for _, id := range order {
		m := byID[id]
		row := p.explainRow(m, rowByID, scopeNames, scopeTotal)
		statusDemoted = statusDemoted || row.StatusFactor != 1.0
		tierDecay = tierDecay || row.RetentionFactor != 1.0
		if r, ok := rank[id]; ok {
			row.Included, row.Rank = true, r
		} else {
			row.Reason = p.explainReason(id, row, rowByID)
		}
		ex.Rows = append(ex.Rows, row)
	}

	ex.Notes = p.explainNotes(ex, factsKnown, statusDemoted, tierDecay)
	var trunc *memory.ExplainTruncation
	ex.Rows, trunc = memory.BoundExplainRows(ex.Rows)
	if trunc != nil {
		ex.Truncation = trunc
		// Ahead of the notes it joins, so the marker reads first: the field beside
		// it is the part that cannot be dropped, and a note list is bounded from
		// the end.
		ex.Notes = append([]string{trunc.Reason}, ex.Notes...)
	}
	return ex
}

// explainRow assembles one candidate's row from the facts the run recorded. A
// candidate with no recorded ranking fact (a retriever that does not record
// them) reports the candidate's own leg ranks and base, and the sentinels for the
// signals only the ranking could have stamped; explainNotes says so.
func (p *pipeline) explainRow(m memory.Memory, rows map[string]memory.Candidate, scopeNames []string, scopeTotal int) memory.ExplainRow {
	req := p.req
	row := memory.ExplainRow{
		ID:                     Token(m.ID),
		Category:               Token(m.Category),
		Content:                Data(memory.ExplainSnippet(m.Content, memory.ExplainSnippetRunes)),
		Retention:              Token(m.Retention),
		ScopeKeysCompared:      append([]string(nil), scopeNames...),
		ScopeKeysComparedTotal: scopeTotal,
		ProvenanceWeight:       memory.ExplainProvenanceOff,
		// Sentinels a row carries until a recorded fact overwrites them.
		FTSRank: -1, VectorRank: -1, VectorScore: -1,
		StatusFactor: 1.0, ScopeMatched: true,
		RowProject:   Token(m.ProjectID),
		ProjectMatch: !BucketUnexpected(m.ProjectID, req.ProjectID) || p.mode == memory.AllProjects,
	}
	if len(scopeNames) == 0 {
		row.ScopeKeysCompared = nil
	}
	c, isCandidate := rows[m.ID]
	if isCandidate {
		row.FTSRank, row.VectorRank, row.VectorScore = c.FTSRank, c.VectorRank, c.VectorScore
		row.RRFScore = c.Base
		row.DecayFactor, row.AgeDays = c.Decay, c.AgeDays
	}
	if f, ok := p.set.RankFacts[m.ID]; ok && f != nil {
		row.FTSRank, row.VectorRank, row.VectorScore = f.FTSRank, f.VectorRank, f.VectorScore
		row.RRFScore = f.Base
		row.StatusFactor = f.StatusFactor
		row.DecayFactor, row.AgeDays = f.Decay, f.AgeDays
		row.ProjectMatch = f.ProjectMatch
		if f.RowProject != "" {
			row.RowProject = Token(f.RowProject)
		}
		row.ScopeMatched = f.ScopeMatched
		row.KeywordReserved = f.KeywordReserved
		row.TookSlotFrom, row.DisplacedBy = tokenOrEmpty(f.TookSlotFrom), tokenOrEmpty(f.DisplacedBy)
		row.FloorDropped, row.FloorScore = f.FloorDropped, f.FloorScore
		row.SupersedePenalty = f.SupersedePenalty
		row.NearDuplicatePenalty = f.NearDuplicatePenalty
		row.SupersededBy = tokens(memory.ClampAttribution(f.SupersededBy))
		row.NearDuplicateOf = tokens(memory.ClampAttribution(f.NearDuplicateOf))
	}
	// A removed near-duplicate loser carries no ranking fact (it was never in the
	// window the ranking facts cover), so the trace's own record names what it
	// lost to. A fact the retriever did record is the same verdict and stands.
	if l, ok := p.losers[m.ID]; ok && len(row.NearDuplicateOf) == 0 {
		row.NearDuplicatePenalty = len(l.LostTo)
		row.NearDuplicateOf = tokens(memory.ClampAttribution(l.LostTo))
	}
	row.RetentionFactor = memory.RetentionDecayFactor(m.Retention, m.Pinned, row.AgeDays)

	// The assembler's own verdicts, from its own stages: validity at the run's
	// clock, the scope verdict the predicate stage reached, and the contributions
	// stages 2 and 4 recorded. A row withheld before a stage never reached it and
	// keeps the zero value, which is also what the stage would have recorded.
	//
	// provenance_weight stays the documented "off": stage 4 pins an inert 1.0 that
	// no score is multiplied by, and publishing it as a weight would read as
	// "provenance was weighed and found neutral" — the contract the tool
	// description and the e2e suite hold is that no weight is applied.
	row.ValidityState, _ = memory.ValidityState(m.ValidFrom, m.ValidUntil, m.VerifiedAt, req.Now)
	row.Confidence = m.Confidence
	if sig, ok := p.trace.Signals[m.ID]; ok {
		row.ScopeMatched = row.ScopeMatched && sig.ScopeMatched
		row.ValidityPenalty = sig.ValidityPenalty
		row.ConfidenceContribution = sig.ConfidenceContribution
		row.ProvenanceContribution = sig.ProvenanceContribution
	}
	return row
}

func tokenOrEmpty(s string) string {
	if s == "" {
		return ""
	}
	return Token(s)
}

func tokens(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = Token(id)
	}
	return out
}

// explainReason says why a candidate is not in the answer, in the words of the
// stage that decided it. A row the pipeline saw is explained by the verdict that
// stage recorded (p.dropped); a row only the retriever saw is explained by the
// ranking fact that left it out — the floor, scope narrowing, or the window.
func (p *pipeline) explainReason(id string, row memory.ExplainRow, rows map[string]memory.Candidate) string {
	if code, ok := p.dropped[id]; ok {
		switch code {
		case validityExpired:
			return "withheld by the validity stage: the memory's validity window has closed (validity_state expired)"
		case validityFuture:
			return "withheld by the validity stage: the memory's validity window has not opened yet (validity_state future)"
		case "category_mismatch":
			return fmt.Sprintf("withheld by the category filter: the memory's category is not %s", Token(p.req.Category))
		case "retention_mismatch":
			return fmt.Sprintf("withheld by the retention filter: the memory's tier is not %s", Token(p.req.Retention))
		case reasonNearDuplicate:
			return fmt.Sprintf("removed as a near-duplicate: the retriever dropped it in favour of %s", strings.Join(tokens(memory.ClampAttribution(p.losers[id].LostTo)), ", "))
		case "scope_contradiction":
			return "excluded by scope: memory scope conflicts with the requested scope"
		case "budget", "slice_budget":
			return fmt.Sprintf("outside the result window: the answer admits only the top %d", p.admitCap())
		case reasonBudgetDropped:
			return fmt.Sprintf("dropped to fit the %d-byte response cap: rows are cut from the bottom of the ranking until the response fits", p.req.Budget.MaxBytes)
		}
		return "withheld by the " + code + " rule"
	}
	switch {
	case row.FloorDropped && row.FTSRank < 0:
		// A dual-leg row is never labelled this way: the floor removes only its
		// vector contribution and the row stays a candidate on its keyword score.
		return fmt.Sprintf("dropped by the vector similarity floor: cosine %.4f is below the minimum %.4f",
			row.FloorScore, p.set.ExplainKnobs.VectorFloor)
	case !row.ScopeMatched:
		return "excluded by scope: memory scope conflicts with the requested scope"
	}
	if _, inRows := rows[id]; inRows {
		// Returned by the retriever, kept by every stage, and still not in the
		// answer: nothing recorded a verdict for it, which the payload says
		// rather than inventing one.
		return "not in the answer, and no stage recorded why"
	}
	return fmt.Sprintf("outside the result window: only the top %d candidates are retrieved", p.trace.Limit)
}

// admitCap is the item count the answer was budgeted to, for the sentence that
// names it: the total cap when there is one, else the sum of the slice caps.
func (p *pipeline) admitCap() int { return itemBound(p.req) }

// explainNotes is the payload's own account of how to read it. The static notes
// qualify the axes the ranking does not score; the dynamic ones name what the
// run actually did (the knobs it ran with, a leg that failed, the window it was
// handed).
func (p *pipeline) explainNotes(ex *memory.SearchExplain, factsKnown, statusDemoted, tierDecay bool) []string {
	var notes []string
	if !ex.VectorAvailable {
		notes = append(notes, "no query embedding available — FTS-only search, so vector scores are absent rather than zero")
	}
	if len(p.req.Scope) > 0 {
		notes = append(notes, "scope is applied inside hybrid window selection; included membership below matches the scoped search")
	}
	if p.req.Category != "" {
		notes = append(notes, "a category filter is applied in the same run: a row it withholds is excluded below with that reason, and a row marked included is in the filtered answer")
	}
	if p.req.Retention != "" {
		notes = append(notes, "a retention filter is applied in the same run: a row it withholds is excluded below with that reason, and a row marked included is in the filtered answer")
	}
	if !factsKnown && len(p.set.Rows) > 0 {
		notes = append(notes, "the retriever recorded no per-candidate ranking facts for this run, so status_factor, the penalties and the reservation fields are their neutral values rather than recorded ones")
	}
	if k := p.set.ExplainKnobs; k.RRFK > 0 {
		notes = append(notes, fmt.Sprintf(
			"rrf_score is the sum, over the legs that retrieved the row, of weight/(%d+rank+1) with weight %g on the keyword leg and %g on the vector leg; the vector similarity floor is %.4f",
			k.RRFK, k.FTSWeight, k.VecWeight, k.VectorFloor))
	}
	for _, name := range []string{"fts", "vector"} {
		if leg := p.set.Legs[name]; leg.Applicable && leg.Attempted && !leg.Available {
			notes = append(notes, fmt.Sprintf("the %s retrieval leg failed (%s): this explanation is incomplete, so a candidate missing from it is not known to be absent", name, Data(leg.Err)))
		}
	}
	if p.windowDisclosure != "" {
		notes = append(notes, p.windowDisclosure)
	}
	if statusDemoted {
		notes = append(notes, "status_factor is applied to the fused score inside window selection, before the cut: multiply rrf_score by status_factor for the score the window actually ranked on (decay_factor then multiplies that)")
	}
	if tierDecay {
		notes = append(notes, "session-tier rows carry a bounded retention decay inside decay_factor, reported per row as retention_factor: a factor above 1.0 never happens, and it falls no lower than 0.5 however old the row is, so a session memory is findable and can never outrank a durable one on recency alone")
	}
	return append(notes, explainAxisNotes()...)
}

// explainAxisNotes disclose, once per payload, the axes the ranking does not
// score and the fields that therefore carry a zero. They are notes rather than
// per-row flags because the answer is the same for every row.
func explainAxisNotes() []string {
	return []string{
		"validity_state is the row's own currency at the search clock and validity_penalty is 0 on every row: the assembler's validity stage withholds an expired or not-yet-valid row instead of ranking it lower, so such a row is excluded with its reason and no score carries a validity factor",
		"confidence and provenance are recorded, not scored: confidence_contribution and provenance_contribution are 0 and provenance_weight is \"off\", because the provenance stage pins an inert weight that no score is multiplied by. Two rows with different confidence therefore ranked identically",
		"supersede_penalty and near_duplicate_penalty are window-scoped, as the retriever applies them over the rows of its result window only: a candidate below the window reports 0. A contradicts edge causes no penalty (supersedes does, and is reported as supersede_penalty with superseded_by naming the superseder), and there is no per-bucket diversity cap on a search: near_duplicate_of names the row each near-duplicate lost to",
	}
}
