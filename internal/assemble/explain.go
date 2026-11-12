package assemble

import (
	"fmt"
	"strings"

	"github.com/wcatz/ghost/internal/memory"
)

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

	rank := make(map[string]int, len(res.Items))
	for i, it := range res.Items {
		rank[it.ID] = i + 1
	}

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
		ex.Notes = append([]string{trunc.Reason}, ex.Notes...)
	}
	return ex
}

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
		FTSRank:                -1,
		VectorRank:             -1,
		VectorScore:            -1,
		StatusFactor:           1.0,
		ScopeMatched:           true,
		RowProject:             Token(m.ProjectID),
		ProjectMatch:           !BucketUnexpected(m.ProjectID, req.ProjectID) || p.mode == memory.AllProjects,
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
	if older, ok := p.movedAhead[m.ID]; ok {
		row.MovedAheadOf = Token(older)
	}
	if l, ok := p.losers[m.ID]; ok && len(row.NearDuplicateOf) == 0 {
		row.NearDuplicateOf = tokens(memory.ClampAttribution(l.LostTo))
	}
	row.RetentionFactor = memory.RetentionDecayFactor(m.Retention, m.Pinned, row.AgeDays)

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
		case reasonContradictionSeparated:
			kept := p.separationKept(id)
			if len(kept) == 1 {
				return fmt.Sprintf("withheld by the conflicts stage: the memory contradicts %s, the row that stage kept", Token(kept[0]))
			}
			return fmt.Sprintf("withheld by the conflicts stage: the memory contradicts %s, the rows that stage kept", strings.Join(tokens(kept), ", "))
		case reasonRelevanceCutoff:
			return fmt.Sprintf("cut by the relevance cutoff: the answer stops where relevance falls off, and this "+
				"memory's fused Base was below %s of the top match's Base", cutoffShareLabel(p.req.RelevanceCutoff))
		case reasonLegConsensus:
			return fmt.Sprintf("withheld by the leg-consensus rule: only one retrieval leg found this memory, and it "+
				"did not rank it first (ranks are 0-based, so a single-leg row is admitted only at vector rank "+
				"<= %d or keyword rank <= %d)", maxSingleLegVectorRank, maxSingleLegFTSRank)
		case reasonNothingClearedBar:
			return fmt.Sprintf("withheld by the no-answer bar: the best vector match in the block scored below "+
				"cosine %.3f (context.no_answer_cosine), so nothing here answers the query", p.req.NoAnswerCosine)
		case "scope_contradiction":
			return "excluded by scope: memory scope conflicts with the requested scope"
		case reasonDiversityDeferred:
			d, ok := p.deferred[id]
			if !ok {
				return "deferred by the diversity stage: the memory's category had filled its share of the window"
			}
			slots := d.slots
			if slots == 0 {
				slots = p.admitCap()
			}
			return fmt.Sprintf("deferred by the diversity stage: category %s had filled its share — no more "+
				"than %d slots for any one category in a %d-row window — so the row was moved behind the "+
				"window and other categories' rows took the slots",
				Token(d.category), d.share, slots)
		case "budget", "slice_budget":
			return fmt.Sprintf("outside the result window: the answer admits only the top %d", p.admitCap())
		case reasonBudgetDropped:
			if p.req.Budget.KeepPinned {
				return fmt.Sprintf("dropped to fit the %d-byte response cap: the lowest-ranked unpinned rows are cut first, until the response fits", p.req.Budget.MaxBytes)
			}
			return fmt.Sprintf("dropped to fit the %d-byte response cap: rows are cut from the bottom of the ranking until the response fits", p.req.Budget.MaxBytes)
		}
		return "withheld by the " + code + " rule"
	}
	switch {
	case row.FloorDropped && row.FTSRank < 0:
		return fmt.Sprintf("dropped by the vector similarity floor: cosine %.4f is below the minimum %.4f",
			row.FloorScore, p.set.ExplainKnobs.VectorFloor)
	case !row.ScopeMatched:
		return "excluded by scope: memory scope conflicts with the requested scope"
	}
	if _, inRows := rows[id]; inRows {
		return "not in the answer, and no stage recorded why"
	}
	return fmt.Sprintf("outside the result window: only the top %d candidates are retrieved", p.trace.Limit)
}

func (p *pipeline) separationKept(withheld string) []string {
	for _, sep := range p.separations {
		if sep.withheld == withheld {
			return sep.kept
		}
	}
	return nil
}

func (p *pipeline) admitCap() int { return itemBound(p.req) }

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

func explainAxisNotes() []string {
	return []string{
		"validity_state is the row's own currency at the search clock and validity_penalty is 0 on every row: the assembler's validity stage withholds an expired or not-yet-valid row instead of ranking it lower, so such a row is excluded with its reason and no score carries a validity factor",
		"confidence and provenance are recorded, not scored: confidence_contribution and provenance_contribution are 0 and provenance_weight is \"off\", because the provenance stage pins an inert weight that no score is multiplied by. Two rows with different confidence therefore ranked identically",
		"supersede_penalty and near_duplicate_penalty are window-scoped, as the retriever applies them over the rows of its result window only: a candidate below the window reports 0. A contradicts edge causes no penalty (supersedes does, and is reported as supersede_penalty with superseded_by naming the superseder): near_duplicate_of names the row each near-duplicate lost to, and no penalty is what a contradicts leg causes because stage 5 withholds the losing side outright",
	}
}
