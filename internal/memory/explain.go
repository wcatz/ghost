package memory

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// explainProvenanceOff is the value provenance_weight carries when no
// provenance multiplier is applied. It is the string "off" rather than a number
// on purpose: a multiplier of 1.0 is a claim that the ranking considered
// provenance and decided it did not matter, which is not what happens — nothing
// reads provenance at all. A reader seeing "off" cannot mistake a missing
// implementation for a neutral one.
const explainProvenanceOff = "off"

// ExplainRow is one candidate's contribution to a search, with every signal
// that moved it stated separately.
//
// Ranks are 0-based within their leg (the same convention RRF uses) so the
// arithmetic shown matches the arithmetic performed. A leg that did not match
// reports -1 rather than 0, because rank 0 is a real first place — an absent
// leg and a top-ranked one must not look alike.
//
// Every SCORING number here is read from the ranking path's own record of the
// candidate (searchTrace), not recomputed from the legs after the fact. A score
// that explain rebuilds is a score that can disagree with the one that ordered
// the results, and an explanation built from one is a diagnosis of a search that
// did not happen. The fields that name a signal the ranking path does NOT apply
// say so with a zero or "off" rather than carrying an invented contribution; the
// payload's notes say which is which.
//
// The one deliberate exception is decay_factor and age_days for a candidate the
// window cut before decayRank ordered it, which are computed here — against the
// ranking's OWN clock, not this call's — because a field is better than a gap and
// the value is the one the ranking would have used. The fields that decide
// membership carry no such exception: a row that did not win a slot has no
// status_factor, no scope verdict and no base read on its behalf, only sentinels
// saying the ranking never reached it.
type ExplainRow struct {
	ID          string  `json:"memory_id"`
	Category    string  `json:"category"`
	Content     string  `json:"content"`
	Included    bool    `json:"included"`
	Rank        int     `json:"rank"`         // 1-based; 0 when excluded
	FTSRank     int     `json:"fts_rank"`     // -1 when the FTS leg had no match
	VectorRank  int     `json:"vector_rank"`  // -1 when the vector leg had no match
	VectorScore float64 `json:"vector_score"` // cosine; -1 when absent
	RRFScore    float64 `json:"rrf_score"`    // base before decay: 1/(K+rank+1) when no vector leg fused, the weighted sum otherwise
	// StatusFactor is the multiplicative resolved/_global demotion the ranking
	// APPLIED to rrf_score before the cut — 1.0, 0.5 or 0.25, and 1.0 when neither
	// applies.
	//
	// 1.0 also covers a row the ranking never scored: the vector floor removed it
	// before fusion, so no demotion ran on it at all. The field means one thing
	// everywhere for that reason — reporting the factor a never-scored row WOULD
	// carry puts a multiplier beside an rrf_score of 0, and the payload's own
	// contract says every number in it is one the ranking used. Whether such a row
	// is a shared one is carried by row_project beside it, and floor_dropped is
	// the field that says nothing was applied.
	StatusFactor         float64 `json:"status_factor"`
	DecayFactor          float64 `json:"decay_factor"`           // category/age AND TIER multiplier the ranking applied to the base (a `session` row carries both halves — see RetentionFactor); for a candidate the window cut before decayRank ordered it, the one it WOULD have applied, measured against the ranking's own clock
	AgeDays              float64 `json:"age_days"`               // as at that same clock, for the same reason
	SupersedePenalty     int     `json:"supersede_penalty"`      // window-scoped as demoteResults applies it; 0 for rows outside the window
	NearDuplicatePenalty int     `json:"near_duplicate_penalty"` // window-scoped and order-sensitive, exactly as DemotionPenalties assigns it
	// Retention is the row's own tier (session, project, persistent) and
	// RetentionFactor is the part of decay_factor it contributed: 1.0 for every
	// tier but session, which is what makes a durable memory's score exactly what
	// it was before tiers existed. Reported as a separate field rather than folded
	// into decay_factor because it is a different question — "how old is this"
	// against "how long do we want this" — and a reader asking why a
	// conversation-scoped row sits low is asking the second one.
	Retention       string  `json:"retention"`
	RetentionFactor float64 `json:"retention_factor"` // bounded tier decay: tau 7d, floor 0.5
	Reason          string  `json:"reason,omitempty"` // why it is absent from the results

	// --- the eligibility axes, as the ranking path decided them ---

	// ProjectMatch is whether the row belongs to the searched project. A
	// _global row is admitted by the legs' project predicate and is NOT a match:
	// it is a shared row, and StatusFactor is what demotes it in a
	// project-scoped search. A cross-project search matches everything, since it
	// expresses no project of its own.
	ProjectMatch bool `json:"project_match"`
	// RowProject is the project the row belongs to, as the leg supplied it.
	// It is what makes ProjectMatch checkable: a reader that sees
	// project_match=false and row_project="_global" knows the row was admitted
	// by the shared-row predicate rather than by a project mismatch.
	RowProject string `json:"row_project,omitempty"`
	// ScopeMatched is the verdict scope narrowing reached, and it agrees with
	// membership by construction because it IS the verdict that decided it — the
	// structural fix for #571, where explain reported rows the tool would exclude
	// because the filter ran after the ranking. Silence is not disagreement: a
	// row that does not mention a requested key is reported matched.
	ScopeMatched bool `json:"scope_matched"`
	// ScopeKeysCompared is the scope key set the narrowing compared — the
	// request's keys, identical on every row, so a reader can see WHICH axis
	// decided rather than only that something did. A row whose own scope names
	// none of them had nothing to compare against and was not excluded by scope;
	// scope_matched says that per row, and this says what the axis was.
	//
	// The list is capped, because the scope object is caller-supplied and as
	// unbounded as a JSON object, and ScopeKeysComparedTotal says how long the
	// list really was. The count is what makes the cap honest: a payload naming
	// sixteen keys of a forty-key scope would otherwise report scope_matched as
	// the verdict the narrowing reached over all forty, and the reader could not
	// tell. The attribution lists beside superseded_by and near_duplicate_of are
	// capped the same way and are counted the same way, for the same reason.
	ScopeKeysCompared      []string `json:"scope_keys_compared,omitempty"`
	ScopeKeysComparedTotal int      `json:"scope_keys_compared_total,omitempty"`
	// KeywordReserved says the row entered the window through the keyword
	// reservation rather than the score cut. Its score is below the cut BY
	// CONSTRUCTION, so this is the only thing that explains its presence, and it
	// is invisible from the numbers otherwise.
	KeywordReserved bool `json:"keyword_reserved,omitempty"`
	// TookSlotFrom names the row whose window slot KeywordReserved took, and
	// DisplacedBy names the reserved row that took this row's slot. Both sides
	// are recorded because the exchange is the interesting fact: neither row's
	// score explains it.
	TookSlotFrom string `json:"took_slot_from,omitempty"`
	DisplacedBy  string `json:"displaced_by,omitempty"`
	// FloorDropped says the vector similarity floor removed this row's VECTOR
	// CONTRIBUTION, and FloorScore is the cosine that did it.
	//
	// Read it as being about the vector leg, not about the row's fate: a row the
	// keyword leg also retrieved is floor-dropped and still IN THE ANSWER, with
	// a non-zero rrf_score, because only its vector term was cut. Check FTSRank to
	// tell the two apart — under 0 means the row is out entirely, and that is the
	// only case where rrf_score is 0. Distinct from VectorRank == -1, which is
	// also what a row the vector leg never matched reports.
	FloorDropped bool    `json:"floor_dropped,omitempty"`
	FloorScore   float64 `json:"floor_score,omitempty"`
	// SupersededBy names the present superseders that sank this row, and
	// NearDuplicateOf the rows it sank below as a near-duplicate loser. A count
	// alone says the row moved; the counterpart is the row a reader has to look
	// at next. Attribution is READ FROM the same edge set the penalty was
	// decided on, so the named row is the one that actually decided it.
	SupersededBy    []string `json:"superseded_by,omitempty"`
	NearDuplicateOf []string `json:"near_duplicate_of,omitempty"`

	// --- signals the ranking path does not apply ---

	// ValidityState is the row's own currency against the search clock, by the
	// one shared rule (memory.ValidityState) the context assembler's validity
	// stage also uses: valid, future, expired, unverified or unset. It is
	// reported because the search ranking does NOT read validity — an expired row
	// is returned like any other — so a caller reading a stale row needs the
	// state from the explanation, and the note says who does drop such rows.
	ValidityState string `json:"validity_state,omitempty"`
	// ValidityPenalty is 0 on every row, and that is the honest value: no stage
	// in the search ranking multiplies by validity. Stage 2 of the assembler
	// DROPS an out-of-window row rather than ranking it lower, so there is no
	// factor for a score to carry. A non-zero value here would describe a
	// ranking that does not exist.
	ValidityPenalty float64 `json:"validity_penalty"`
	// Confidence is the stored provenance confidence, and
	// ConfidenceContribution is the score delta a bounded multiplier applied —
	// which is 0, because no such multiplier exists. Both are reported: the
	// column is readable and a reader comparing rows needs to see that two rows
	// with different confidence ranked identically, rather than inferring it
	// from a field that is absent.
	Confidence             *float64 `json:"confidence,omitempty"`
	ConfidenceContribution float64  `json:"confidence_contribution"`
	// ProvenanceWeight is "off": no score anywhere in the search path is
	// multiplied by provenance. See explainProvenanceOff.
	ProvenanceWeight string `json:"provenance_weight"`
	// ProvenanceContribution is 0 for the same reason as
	// ConfidenceContribution.
	ProvenanceContribution float64 `json:"provenance_contribution"`
}

// SearchExplain is the diagnosis for one search: which legs ran, what each
// candidate scored, and why anything was left out.
//
// Membership is never re-derived here. Rows are marked included by asking the
// same scoped or unscoped production search the formatted path uses, so an
// explanation describes the ranking Ghost actually produced for that query.
// Recomputing membership locally would let the explanation drift from the
// behavior it explains.
type SearchExplain struct {
	ProjectID       string            `json:"project_id"`
	Query           string            `json:"query"`
	Limit           int               `json:"limit"`
	VectorAvailable bool              `json:"vector_available"`
	Scope           map[string]string `json:"scope,omitempty"` // requested scope applied to membership
	Notes           []string          `json:"notes,omitempty"`
	Rows            []ExplainRow      `json:"rows"`
	// Truncation is set when the payload hit its size budget. It is a field
	// rather than a note because a note is prose a reader may skip, and a short
	// candidate list that LOOKS complete is the failure this prevents: an agent
	// checking a row that "was not even a candidate" has to be able to tell that
	// the explanation never looked.
	Truncation *ExplainTruncation `json:"truncation,omitempty"`
}

// ExplainTruncation records that the payload was cut to fit the documented
// budget, and says what was cut and why. RowsOmitted counts candidate rows the
// budget would not carry; MaxRows is the budget itself, published so a reader can
// judge whether the cut could plausibly have hidden the row it cares about.
type ExplainTruncation struct {
	RowsOmitted int    `json:"rows_omitted"`
	MaxRows     int    `json:"max_rows"`
	Reason      string `json:"reason"`
}

// explainMaxRows is the documented size budget for one explanation, in rows.
//
// The candidate pool is the union of what both legs returned, so it grows with
// the caller's limit rather than with the corpus: each leg fetches limit*2, so a
// hybrid search over a well-matched corpus produces a candidate set four times
// the window. The window itself is capped (the tool's retrieval-window ceiling
// is 100 rows), which is what makes a fixed row budget possible at all: 150
// holds every row a search can return, plus the candidates nearest the cut, and
// cuts only the far tail of the union beyond that. At the default limit of ten
// the union is at most 40 rows and the budget never engages.
//
// It spends itself on the CANDIDATES and never on the answer: rows the search
// returned are always kept, in their own order, and the rows dropped are the ones
// that were already excluded. A caller diagnosing a missing result is looking
// for a row the search did not return, and a budget that could drop those would
// spend itself exactly where it is needed least.
const explainMaxRows = 150

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
	// The same stamp searchHybridLegs applies for membership, so the factor
	// reported below is the one the search used rather than a re-derivation.
	p.ProjectID = projectID
	// The trace the ranking path writes as it ranks. Every score, factor and
	// verdict below is READ from it rather than recomputed here, which is what
	// makes this a record of the search instead of a second attempt at it.
	p.trace = newSearchTrace(scope)

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
	identity := s.embeddingIdentity
	s.mu.RUnlock()
	p.MinSimilarity = floor

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ex, fmt.Errorf("begin explain snapshot: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Every knob the trace store reads has to be carried across, or the trace
	// describes a search this process would not run: without the identity the
	// vector leg here scores the foreign vectors the real search excluded, and
	// the per-row ranks it reports describe a result set the caller cannot
	// reproduce — which is the one thing an explain call promises. The
	// foreign-warning gate travels for the same reason in reverse: shared, one
	// explain during a re-embed costs no second copy of the warning the real
	// search already logged.
	traceStore := &Store{
		db:                  s.db,
		snapshot:            tx,
		logger:              s.logger,
		demotionThreshold:   demotionThreshold,
		vectorMinSimilarity: floor,
		embeddingIdentity:   identity,
		foreignWarned:       s.foreignWarned,
		// Shares the corpus scratch pool: this store runs the same vector leg,
		// and a pool of its own would allocate per explain run and go with it.
		scratch: s.scratch,
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

	// The raw leg is needed for one reason only: a candidate the vector floor
	// removed never reached fusion, so it is in neither the window nor the
	// trace's scored rows, and without the raw leg it would be invisible — a
	// distinct outcome ("your query matched nothing strongly enough") reported
	// as silence. Its SCORE reaches the payload through the trace (floor_score,
	// stamped where the floor is applied), so nothing is re-read from it here.
	fts, rawVec := legs.fts, legs.vec
	finalRank := make(map[string]int, len(final))
	for i, m := range final {
		finalRank[m.ID] = i + 1
	}

	// Candidate union: everything any leg surfaced, plus everything returned.
	// The trace's own row set is deliberately NOT the union's basis — it holds
	// only the candidates the ranking path examined, and a floor-dropped row is
	// stamped rather than scored, so the legs are still the honest source for
	// "which rows existed at all".
	seen := map[string]bool{}
	ids := make([]string, 0, len(fts)+len(rawVec)+len(finalRank))
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
		// A payload with no rows at all, so none of the per-row notes have
		// anything to qualify — the axis notes explain what a zero in a ROW's
		// field means, and there are no fields. Deliberately returned before
		// explainAxisNotes rather than after, so a caller can tell an empty
		// search from a searched one by the note set as well as by Rows.
		return ex, nil
	}

	candidates, err := traceStore.GetByIDs(ctx, ids)
	if err != nil {
		return ex, fmt.Errorf("hydrate candidates: %w", err)
	}
	byID := make(map[string]Memory, len(candidates))
	for _, m := range candidates {
		byID[m.ID] = m
	}

	// The clock the ranking ordered by, so a row's age and decay are measured
	// against the search rather than against this loop. They differ by however
	// long hydration took, which is invisible on a young row and visible on a
	// year-old one.
	now := p.trace.now
	if now.IsZero() {
		// No window was selected, so nothing ordered by a clock. The candidates
		// still need an age and a factor, and any instant is as good as another
		// when the number is not the one that ranked anything.
		now = time.Now().UTC()
	}

	// When the vector leg contributes nothing, ranking used the unweighted
	// keyword base: keywordOnlyParams sets FTSWeight=1, VecWeight=0, so the fused
	// score is 1/(K+rank+1). Reporting the weighted form there would show a
	// number 0.3x the one that actually ranked the results.
	//
	// The verdict is the ranking path's own, NOT a second filterVectorFloor call
	// on the same input. Re-applying the floor here would be the one remaining
	// parallel computation in this file, and it would quietly describe a floor the
	// ranking no longer applies the moment a stage adjusts the effective threshold
	// before calling it.
	//
	// The flag is set on both of the ranking path's exits, so it is true for the
	// nil-query-vector case as well as the floor-refused-everything case — a search
	// that never had an embedding reports an unweighted base too, and saying so is
	// the only way a reader can tell that base apart from a weak vector match.
	if p.trace.keywordOnlyBase {
		ex.Notes = append(ex.Notes, "no vector matches survived — ranking used the unweighted FTS base score, so rrf_score reports that base rather than a weighted sum")
	}
	// The tier's decay is named when some candidate carries one, and only then: a
	// corpus with no session row has no tier decay, and a diagnosis that
	// describes a signal nothing ranked is one more thing to read past. The bound
	// is in the sentence because the number alone does not say which end of it a
	// row is at.
	tierDecay := false
	for _, id := range ids {
		if m, ok := byID[id]; ok && RetentionDecayFactor(m.Retention, m.Pinned, ageDays(m.CreatedAt, time.Now().UTC())) != 1.0 {
			tierDecay = true
			break
		}
	}
	if tierDecay {
		ex.Notes = append(ex.Notes, "session-tier rows carry a bounded retention decay inside decay_factor, reported per row as retention_factor: a factor above 1.0 never happens, and it falls no lower than 0.5 however old the row is, so a session memory is findable and can never outrank a durable one on recency alone")
	}

	// The request's scope key set is the same on every row, so it is clamped once
	// here rather than per row — the copy is what keeps the rows from sharing a
	// backing array, and doing it once makes that obvious.
	scopeKeys, scopeKeysTotal := clampScopeKeys(p.trace.scopeKeys)

	statusDemoted := false
	for _, id := range ids {
		m, ok := byID[id]
		if !ok {
			continue // raced with a delete; not diagnosable, skip
		}
		t, traced := p.trace.lookup(id)
		row := ExplainRow{
			ID:                     id,
			Category:               m.Category,
			Content:                explainSnippet(m.Content, 120),
			ScopeKeysCompared:      scopeKeys,
			ScopeKeysComparedTotal: scopeKeysTotal,
			ProvenanceWeight:       explainProvenanceOff,
		}
		if traced {
			// Every scoring fact is the ranking path's own, and the sentinels on a
			// row the path never scored are the ones below — a floor-dropped
			// candidate was stamped, not scored, and it keeps them.
			row.FTSRank, row.VectorRank, row.VectorScore = t.FTSRank, t.VectorRank, t.VectorScore
			row.RRFScore = t.Base
			row.StatusFactor = t.StatusFactor
			row.DecayFactor, row.AgeDays = t.Decay, t.AgeDays
			if t.Decay == 0 {
				// The row never reached decayRank, so no ordering was taken on it
				// and there is no factor the ranking used. What is reported instead
				// is the factor it WOULD carry, measured at the ranking's own clock
				// rather than this loop's. DecayFactor has a 0.15 floor, so a
				// recorded 0.0 is unambiguously "never ordered" rather than a real
				// value — the two are told apart by that floor, not by a sentinel
				// the field never had.
				row.AgeDays = ageDays(m.CreatedAt, now)
				row.DecayFactor = DecayFactor(m.Category, m.Retention, m.Pinned, row.AgeDays)
			}
			row.ProjectMatch, row.RowProject = t.ProjectMatch, t.RowProject
			row.ScopeMatched = t.ScopeMatched
			row.KeywordReserved, row.TookSlotFrom, row.DisplacedBy = t.KeywordReserved, t.TookSlotFrom, t.DisplacedBy
			row.FloorDropped, row.FloorScore = t.FloorDropped, t.FloorScore
			row.SupersedePenalty = t.SupersedePenalty
			row.NearDuplicatePenalty = t.NearDuplicatePenalty
			row.SupersededBy = clampAttribution(t.SupersededBy)
			row.NearDuplicateOf = clampAttribution(t.NearDuplicateOf)
		} else {
			// Unreachable while the trace covers every leg row, which it does: the
			// floor site stamps the rows fusion never saw. Kept because a nil
			// dereference in a diagnostic path is a worse failure than a
			// conservative row.
			//
			// It is NOT a pure sentinel row, and the one exception this file
			// documents applies here too: the PROJECT axis and the decay pair are
			// read off the hydrated row, against the ranking's clock. They are
			// statements about the row rather than about a ranking decision, so a
			// row keeps them whether or not the ranking reached it. What this
			// branch does NOT do is invent a ranking verdict: no base, no status
			// factor, no scope verdict, no attribution.
			//
			// Every sentinel is ASSIGNED rather than left to the Go zero value,
			// because the zero is outside the documented domain of each field:
			// StatusFactor 0 is not a value statusDemotionFactor can return, and
			// the statusDemoted flag below would read it as a demotion and
			// disclose one. No demotion ran on a row nothing scored, so the applied
			// factor is 1.0.
			row.FTSRank, row.VectorRank, row.VectorScore = -1, -1, -1
			row.StatusFactor = 1.0
			row.AgeDays = ageDays(m.CreatedAt, now)
			row.DecayFactor = DecayFactor(m.Category, m.Retention, m.Pinned, row.AgeDays)
			row.RowProject = m.ProjectID
			row.ProjectMatch = p.ProjectID == "" || m.ProjectID == p.ProjectID
			row.ScopeMatched = true
		}
		row.Retention = m.Retention
		row.RetentionFactor = RetentionDecayFactor(m.Retention, m.Pinned, row.AgeDays)
		statusDemoted = statusDemoted || row.StatusFactor != 1.0

		// Validity is reported, never applied: the search ranking does not read
		// it, so the state is the row's own currency and the penalty is zero
		// because no score anywhere here carries one. Read through the SAME rule
		// the assembler's validity stage uses, so the two surfaces cannot answer
		// the question differently — each at its own clock, which is the one
		// remaining difference between them and the only one that can move a
		// boundary row's state.
		row.ValidityState, _ = ValidityState(m.ValidFrom, m.ValidUntil, m.VerifiedAt, now)
		row.Confidence = m.Confidence

		if rank, isFinal := finalRank[id]; isFinal {
			row.Included = true
			row.Rank = rank
			ex.Rows = append(ex.Rows, row)
			continue
		}

		// Excluded. Demotions in this system are membership-preserving, so
		// absence is the vector floor, the scope constraint, or the result
		// window, in the same order production applies them — and every one of
		// those verdicts is now the ranking path's own, so the reason and the
		// membership cannot come from different decisions.
		switch {
		case row.FloorDropped && row.FTSRank < 0:
			// A floor-dropped candidate that the keyword leg also missed. A dual-leg
			// row is never labelled this way: the floor removes only its vector
			// contribution and the row stays a candidate on its keyword score.
			row.Reason = fmt.Sprintf("dropped by the vector similarity floor: cosine %.4f is below the minimum %.4f",
				row.FloorScore, float64(p.MinSimilarity))
		case !row.ScopeMatched:
			row.Reason = "excluded by scope: memory scope conflicts with the requested scope"
		default:
			row.Reason = fmt.Sprintf("outside the result window: only the top %d are returned", limit)
		}
		ex.Rows = append(ex.Rows, row)
	}
	if statusDemoted {
		ex.Notes = append(ex.Notes, "status_factor is applied to the fused score inside window selection, before the cut: multiply rrf_score by status_factor for the score the window actually ranked on (decay_factor then multiplies that)")
	}
	// The budget is applied last, so it sees every row the diagnosis produced,
	// and the marker is attached before the notes so it reads first: a note list
	// is bounded from the end, and the field beside it is the part that cannot
	// be dropped.
	ex.Rows, ex.Truncation = boundExplainRows(ex.Rows)
	if ex.Truncation != nil {
		ex.Notes = append(ex.Notes, ex.Truncation.Reason)
	}
	ex.Notes = append(ex.Notes, explainAxisNotes()...)
	return ex, nil
}

// explainAxisNotes disclose, once per payload, the axes the ranking path does
// NOT act on and the fields that therefore carry a zero. They are notes and not
// per-row flags because the answer is the same for every row: a reader who finds
// confidence_contribution = 0 needs to know that zero means "no multiplier
// exists", not "the multiplier measured zero", and the cheapest place to say that
// once is the payload.
//
// They are appended LAST so a caller that bounds the note list keeps the ones
// that qualify the result. Nothing renders notes from a length-capped position,
// so the order here is documentation of intent rather than a mechanism.
func explainAxisNotes() []string {
	return []string{
		"validity_state is the row's own currency against the search clock and validity_penalty is 0: the search ranking does not read validity, so an expired or future row is returned like any other. The context assembler drops such a row at its validity stage instead of ranking it lower — use ghost_memory_search without explain to get that answer",
		"confidence and provenance are recorded, not scored: confidence_contribution and provenance_contribution are 0 and provenance_weight is \"off\" because no stage in the search ranking multiplies by either. Two rows with different confidence therefore ranked identically, and the columns are reported so that is visible rather than inferred",
		"a contradicts edge causes no penalty in this ranking (supersedes does, and is reported as supersede_penalty with superseded_by naming the superseder), and there is no per-bucket diversity cap here: near_duplicate_penalty with near_duplicate_of names the row each near-duplicate lost to",
	}
}

// boundExplainRows applies the documented size budget, and records the cut when
// it applies one.
//
// The budget spends itself on candidates, never on the answer. Every included row
// is kept — regardless of where it falls in the list, which is not a reliable
// proxy for it, because the list runs leg by leg and a vector-leg rank-0 row sits
// after every keyword-leg row however it ranked. The rows dropped are excluded
// candidates, in list order, which is NOT a rank order: the list is every
// keyword-leg id, then every vector-leg id, then the final ids, so a positional
// cut takes a keyword-leg exclusion before a vector-leg one regardless of which
// came closer to the cut. Nothing in the payload claims otherwise, and no ordering
// claim survives that the list's construction does not support. The surviving rows
// keep their original order, so the payload looks the same with a budget as
// without one, only shorter.
//
// A caller that asks for a window wider than the budget gets an OVER-budget
// payload rather than a truncated answer, and MaxRows reports the true row count
// in that case. The tool cannot reach that state — it clamps the window to 100 —
// but returning a marker claiming a 150-row cap on a 400-row payload would be the
// one way this could mislead, and the alternative (dropping a returned row) is the
// guarantee the budget exists not to break.
func boundExplainRows(rows []ExplainRow) ([]ExplainRow, *ExplainTruncation) {
	if len(rows) <= explainMaxRows {
		return rows, nil
	}
	included := make([]bool, len(rows))
	includedCount := 0
	for i, r := range rows {
		if r.Included {
			included[i] = true
			includedCount++
		}
	}
	// The tool's window ceiling is 100 rows, so the answer always fits inside a
	// 150-row budget and the first pass never has to drop one. The clamp is
	// stated rather than assumed, so a future larger window cannot quietly start
	// losing returned rows — it makes the payload over budget and says so.
	room := explainMaxRows - min(includedCount, explainMaxRows)
	keep := make([]bool, len(rows))
	for i := range rows {
		if included[i] {
			keep[i] = true
		}
	}
	omitted := 0
	for i := range rows {
		if keep[i] || room == 0 {
			continue
		}
		keep[i] = true
		room--
	}
	kept := make([]ExplainRow, 0, len(rows))
	keptCount := 0
	for i, r := range rows {
		if keep[i] {
			kept = append(kept, r)
			keptCount++
			continue
		}
		omitted++
	}
	// The reason names the true row count, and says which of the two states this
	// is: a cut that kept the whole answer, or an answer that was over budget on
	// its own and so had no room for a single excluded candidate.
	reason := fmt.Sprintf(
		"the explanation was truncated to its documented %d-row budget: %d excluded candidates were omitted "+
			"and all %d rows the search returned were kept, so a row absent from this payload was either never a "+
			"candidate or was cut by the budget",
		explainMaxRows, omitted, includedCount)
	if keptCount > explainMaxRows {
		reason = fmt.Sprintf(
			"the %d rows the search returned exceed the documented %d-row budget on their own, so this payload "+
				"is %d rows long and every excluded candidate (%d of them) was dropped: the budget is not applied to "+
				"rows in the answer, because a caller that asked for this window needs the window back",
			includedCount, explainMaxRows, keptCount, omitted)
	}
	// MaxRows is what the payload ACTUALLY carries, not the budget it aimed for.
	// They differ only when the answer alone exceeds the budget, which the tool
	// cannot produce but an in-process caller can; reporting the budget there
	// would be a cap claim the payload violates on its face.
	trunc := &ExplainTruncation{
		RowsOmitted: omitted,
		MaxRows:     max(keptCount, explainMaxRows),
		Reason:      reason,
	}
	return kept, trunc
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
