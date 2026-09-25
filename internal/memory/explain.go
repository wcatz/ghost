package memory

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ExplainRow is one candidate's contribution to a search, with every signal
// that moved it stated separately.
//
// Ranks are 0-based within their leg (the same convention RRF uses) so the
// arithmetic shown matches the arithmetic performed. A leg that did not match
// reports -1 rather than 0, because rank 0 is a real first place — an absent
// leg and a top-ranked one must not look alike.
type ExplainRow struct {
	ID                   string  `json:"memory_id"`
	Category             string  `json:"category"`
	Content              string  `json:"content"`
	Included             bool    `json:"included"`
	Rank                 int     `json:"rank"`                   // 1-based; 0 when excluded
	FTSRank              int     `json:"fts_rank"`               // -1 when the FTS leg had no match
	VectorRank           int     `json:"vector_rank"`            // -1 when the vector leg had no match
	VectorScore          float64 `json:"vector_score"`           // cosine; -1 when absent
	RRFScore             float64 `json:"rrf_score"`              // base before decay: 1/(K+rank+1) when no vector leg fused, the weighted sum otherwise
	DecayFactor          float64 `json:"decay_factor"`           // category/age multiplier applied to the base
	AgeDays              float64 `json:"age_days"`               //
	SupersedePenalty     int     `json:"supersede_penalty"`      // window-scoped as demoteResults applies it; 0 for rows outside the window
	NearDuplicatePenalty int     `json:"near_duplicate_penalty"` // window-scoped and order-sensitive, exactly as DemotionPenalties assigns it
	Reason               string  `json:"reason,omitempty"`       // why it is absent from the results
}

// SearchExplain is the diagnosis for one search: which legs ran, what each
// candidate scored, and why anything was left out.
//
// Membership is never re-derived here. Rows are marked included by asking the
// same scoped or unscoped production search the formatted path uses, so an
// explanation describes the ranking Ghost actually produced for that query.
type SearchExplain struct {
	ProjectID       string            `json:"project_id"`
	Query           string            `json:"query"`
	Limit           int               `json:"limit"`
	VectorAvailable bool              `json:"vector_available"`
	Scope           map[string]string `json:"scope,omitempty"` // requested scope applied to membership
	Notes           []string          `json:"notes,omitempty"`
	Rows            []ExplainRow      `json:"rows"`
}

// ExplainSearch runs the production unscoped search and reports how each
// candidate got its score. It is kept as the compatibility entry point for
// callers that do not have a scope constraint.
func (s *Store) ExplainSearch(ctx context.Context, projectID, query string, queryVec []float32, limit int) (SearchExplain, error) {
	return s.ExplainSearchScoped(ctx, projectID, query, queryVec, limit, nil)
}

// ExplainSearchScoped is the production explain entry point. It uses the same
// scoped search and window selection as formatted retrieval, then explains the
// resulting membership without re-deriving it locally.
func (s *Store) ExplainSearchScoped(ctx context.Context, projectID, query string, queryVec []float32, limit int, scope map[string]string) (SearchExplain, error) {
	p := DefaultSearchParams()
	p.MinSimilarity = s.vectorMinSimilarityFloor()
	p.Scope = scope

	ex := SearchExplain{
		ProjectID:       projectID,
		Query:           query,
		Limit:           limit,
		VectorAvailable: queryVec != nil,
		Scope:           scope,
	}
	if queryVec == nil {
		ex.Notes = append(ex.Notes, "no query embedding available — FTS-only search, so vector scores are absent rather than zero")
	}
	if len(scope) > 0 {
		ex.Notes = append(ex.Notes, "scope is applied inside hybrid window selection; included membership below matches the scoped search")
	}

	// Keep every explain read on one SQLite snapshot. The production search and
	// its diagnostic leg reads must describe the same database state even when
	// another process writes between individual queries.
	s.mu.RLock()
	floor := s.vectorMinSimilarity
	demotionThreshold := s.demotionThreshold
	s.mu.RUnlock()
	p.MinSimilarity = floor

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ex, fmt.Errorf("begin explain snapshot: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	traceStore := &Store{
		db:                  s.db,
		snapshot:            tx,
		logger:              s.logger,
		demotionThreshold:   demotionThreshold,
		vectorMinSimilarity: floor,
	}

	// Membership comes from the same production search the formatted path uses,
	// and the diagnostics reuse the legs that search already fetched. OpenDB
	// caps the pool at one connection, so a second FTS scan and a second full
	// embedding scan inside this transaction would each be time a concurrent
	// writer could not have the connection — the trace would be the only thing
	// holding it, and nothing here needs the rows fetched twice.
	final, legs, err := traceStore.searchHybridLegs(ctx, projectID, query, queryVec, limit, p)
	if err != nil {
		return ex, fmt.Errorf("search: %w", err)
	}

	// Both the raw and floor-filtered vector legs are needed: a candidate
	// dropped by the floor is a distinct, diagnosable outcome ("your query
	// matched nothing strongly enough"), and it is invisible if only the
	// filtered leg is kept.
	fts, rawVec := legs.fts, legs.vec
	vec := filterVectorFloor(rawVec, p.MinSimilarity)

	ftsRank := make(map[string]int, len(fts))
	for i, m := range fts {
		if _, seen := ftsRank[m.ID]; !seen {
			ftsRank[m.ID] = i
		}
	}
	vecRank := make(map[string]int, len(vec))
	vecScore := make(map[string]float64, len(vec))
	for i, v := range vec {
		vecRank[v.MemoryID] = i
		vecScore[v.MemoryID] = float64(v.Score)
	}
	rawRank := make(map[string]int, len(rawVec))
	rawScore := make(map[string]float64, len(rawVec))
	for i, v := range rawVec {
		rawRank[v.MemoryID] = i
		rawScore[v.MemoryID] = float64(v.Score)
	}
	finalRank := make(map[string]int, len(final))
	for i, m := range final {
		finalRank[m.ID] = i + 1
	}

	// Candidate union: everything any leg surfaced, plus everything returned.
	seen := map[string]bool{}
	ids := make([]string, 0, len(ftsRank)+len(rawRank)+len(finalRank))
	add := func(id string) {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, m := range fts {
		add(m.ID)
	}
	for _, v := range rawVec {
		add(v.MemoryID)
	}
	for _, m := range final {
		add(m.ID)
	}
	if len(ids) == 0 {
		return ex, nil
	}

	candidates, err := traceStore.GetByIDs(ctx, ids)
	if err != nil {
		return ex, fmt.Errorf("hydrate candidates: %w", err)
	}
	byID := make(map[string]Memory, len(candidates))
	pinned := make(map[string]bool, len(candidates))
	for _, m := range candidates {
		byID[m.ID] = m
		pinned[m.ID] = m.Pinned
	}

	// Penalties are window-scoped and order-sensitive, exactly as the search
	// applies them. demoteResults runs over the returned window in returned
	// order, and DemotionPenalties decides which member of a near-duplicate
	// pair loses from its position in that slice (rank[b] > rank[a]) — so
	// passing anything else both widens the set and changes the verdict.
	finalIDs := make([]string, len(final))
	for i, m := range final {
		finalIDs[i] = m.ID
	}
	supersede, supErr := SupersedePenalties(ctx, traceStore.queryDB(), finalIDs)
	nearDup, nearErr := DemotionPenalties(ctx, traceStore.queryDB(), finalIDs, pinned, demotionThreshold)
	if supErr != nil {
		supersede = nil
	}
	if nearErr != nil {
		nearDup = nil
	}

	// SearchHybrid reports an unweighted base when the vector leg contributes
	// nothing: it passes a nil score map to decayRank, which synthesises
	// 1/(K+rank+1). Reporting the weighted form there would show a number
	// 0.3x the one that actually ranked the results.
	ftsOnly := len(vec) == 0
	if ftsOnly {
		ex.Notes = append(ex.Notes, "no vector matches survived — ranking used the unweighted FTS base score, so rrf_score reports that base rather than a weighted sum")
	}

	now := time.Now().UTC()
	for _, id := range ids {
		m, ok := byID[id]
		if !ok {
			continue // raced with a delete; not diagnosable, skip
		}
		row := ExplainRow{
			ID:       id,
			Category: m.Category,
			Content:  explainSnippet(m.Content, 120),
			FTSRank:  -1, VectorRank: -1, VectorScore: -1,
			SupersedePenalty:     supersede[id],
			NearDuplicatePenalty: nearDup[id],
			AgeDays:              ageDays(m.CreatedAt, now),
		}
		row.DecayFactor = DecayFactor(m.Category, m.Pinned, row.AgeDays)

		if r, hit := ftsRank[id]; hit {
			row.FTSRank = r
			if ftsOnly {
				// Matches decayRank's synthesized base: SearchHybrid passes a
				// nil score map on this path, so ranking used 1/(K+rank+1)
				// with no leg weight applied.
				row.RRFScore += 1.0 / float64(p.RRFK+r+1)
			} else {
				row.RRFScore += p.FTSWeight / float64(p.RRFK+r+1)
			}
		}
		if r, hit := vecRank[id]; hit {
			row.VectorRank = r
			row.VectorScore = vecScore[id]
			row.RRFScore += p.VecWeight / float64(p.RRFK+r+1)
		}

		if rank, isFinal := finalRank[id]; isFinal && (len(scope) == 0 || ScopeMatches(m.Scope, scope)) {
			row.Included = true
			row.Rank = rank
			ex.Rows = append(ex.Rows, row)
			continue
		}

		// Excluded. Demotions in this system are membership-preserving, so
		// absence is the vector floor (for a vector-only candidate), the scope
		// constraint, or the result window, in the same order production
		// applies them. A vector floor removes only the vector contribution of
		// a dual-leg FTS candidate, so that row must not be labeled as if the
		// whole memory had been dropped.
		_, onFloor := rawRank[id]
		_, survived := vecRank[id]
		_, ftsHit := ftsRank[id]
		switch {
		case onFloor && !survived && !ftsHit:
			row.Reason = fmt.Sprintf("dropped by the vector similarity floor: cosine %.4f is below the minimum %.4f",
				rawScore[id], float64(p.MinSimilarity))
		case len(scope) > 0 && !ScopeMatches(m.Scope, scope):
			row.Reason = "excluded by scope: memory scope conflicts with the requested scope"
		default:
			row.Reason = fmt.Sprintf("outside the result window: only the top %d are returned", limit)
		}
		ex.Rows = append(ex.Rows, row)
	}
	return ex, nil
}

// explainSnippet shortens content for a diagnostic listing. Explanations are
// read by a human or an agent debugging a query, so the identifying opening
// words matter more than the full text.
func explainSnippet(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
