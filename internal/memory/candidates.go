package memory

// this package, never the reverse.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ProjectMode is the storage-level distinction the retrieval SQL requires: a
// project search also admits `_global` rows, an unresolved project admits only
type ProjectMode string

const (
	ProjectScoped ProjectMode = "scoped"       // project_id = ? OR '_global'
	GlobalOnly    ProjectMode = "global_only"  // project_id = '_global'
	AllProjects   ProjectMode = "all_projects" // no project predicate
)

// Condition selects which retrieval legs run. It is part of the retriever
// contract rather than a SearchParams field because a nil query vector already
type Condition string

const (
	CondHybrid     Condition = "hybrid"      // both legs, fused
	CondFTSOnly    Condition = "fts_only"    // keyword leg alone
	CondVectorOnly Condition = "vector_only" // vector leg alone; requires a query vector
)

// CandidateRequest is one retrieval: what to fetch, from where, and the clock
// every time-dependent decision is made against. Now is required and is used
type CandidateRequest struct {
	ProjectID string
	Mode      ProjectMode
	Query     string
	QueryVec  []float32
	Scope     map[string]string
	Category  string
	Condition Condition
	Params    SearchParams
	Now       time.Time
	AsOf      *time.Time
	Fetch     Fetch
	Explain   bool
	// its policies discarded and a window its caller never stated.
	Passive []SlicePolicy
}

// Fetch is the retrieval depth. FTSTopK and VectorTopK are the per-leg depths
// (production fetches twice the window from each leg); Limit is the window
type Fetch struct {
	FTSTopK, VectorTopK int
	Limit               int // Candidates returns a wider untrimmed set
}

// SlicePolicy is one passive bucket's selection rules: how wide to fetch, in
// what order, which categories are behavioural, and whether a near-duplicate
type SlicePolicy struct {
	Bucket                string
	Order                 string // decay or pinned_importance_updated
	TwoPass               bool
	BehaviorFloor         int
	BehaviorCategories    []string
	CategoryWeights       map[string]float64
	CategoryCaps          map[string]int
	OverFetch             int
	DemotionThreshold     float64
	IncludeGlobal         bool
	ItemCap               int
	DemoteOnlyWhenOverCap bool
	// passive policy that must not repeat a row the project bucket already
	ExcludeSeen       bool
	DropDemotedLosers bool
}

// DroppedLoser is one near-duplicate loser the retriever removed rather than
// ranked last — from a collapsed query window or a passive bucket — with the
type DroppedLoser struct {
	Candidate
	LostTo   []string
	StandIns []string
}

// CandidateSet is one retrieval's rows plus the facts about how they were
// retrieved. Widened reports that the set is larger than the requested window,
type CandidateSet struct {
	Rows []Candidate
	// the removal happened over a window whose edges it never saw. Carrying them
	DroppedLosers    []DroppedLoser
	Edges            []LinkEdge
	EdgesStatus      EdgeStatus
	Legs             map[string]LegStatus
	Widened          bool
	Unrecorded       int
	ValidityExcluded int
	// pinned rows a cap cut when it never saw some of them. The window is the
	PinnedBeyond map[string]int
	// window must count them as inside it, or it counts them twice.
	WindowExtra map[string]int

	// buys, and with it false these fields are never touched, so the plain

	// rows the window and its tail never reached; together with Rows and
	Excluded []Memory
	// than read as a candidate that never existed. Hydrated, as Rows and
	FloorDropped []Memory
	// entry is a row the ranking genuinely never scored.
	RankFacts       map[string]*RankFact
	ExplainKnobs    ExplainKnobs
	floorDroppedIDs []string
}

// ExplainKnobs is the resolved fusion configuration one ranking ran under. It is
// carried on the CandidateSet so the explain projection can name what a score
type ExplainKnobs struct {
	RRFK                 int
	FTSWeight, VecWeight float64
	VectorFloor          float64
}

// Candidate is one hydrated row with the scoring facts fusion produced for it.
// The facts are carried rather than re-derived, so a trace cannot report a
type Candidate struct {
	Memory
	// absent leg must not look like one.
	FTSRank, VectorRank int
	VectorScore         float64
	FetchedBy           string
	// keep-forever (retention-exempt) target, which the demotion never sinks, is
	SupersededBy       []string
	MaybeReplacedBy    []string
	FloorReserved      bool
	Base, Decay, Score float64
	AgeDays            float64
	KeywordReserved    bool
	// It is false whenever the vector leg is not in play (no embedder, no query
	Unembedded bool
	Evidence   EvidenceCounts
}

// LinkEdge is one edge between two candidates. Stages 5 and 6 read it to
// reorder and record conflicts; the assembler never queries for it itself.
type LinkEdge struct {
	From, To, Relation string
	Strength           float64
}

// LegStatus is what one retrieval leg did. CoverageComplete is the reason an
// empty answer may claim absence: a leg that skipped unembedded or
type LegStatus struct {
	Attempted, Available bool
	Err                  string
	Truncated            bool
	Applicable           bool
	// claim must not make one, which is why the field is false rather than
	Expected, Indexed, Eligible, DimMismatch, Unembedded int
	CoverageComplete                                     bool
}

// EdgeStatus is the edge lookup's own outcome, kept separate from the rows so a
// failed lookup can never read as "no edges exist": "ok" (the query ran, and
type EdgeStatus struct {
	Status, Err string
	// pair whose endpoints land in different chunks is never read, and a caller
	Chunks int
}

const (
	edgesOK          = "ok"
	edgesUnavailable = "unavailable"
	edgesErr         = "err"
	// case: memory_links records when an edge was invalidated, never what the
	edgesNotApplicable = "not_applicable"
)

// ErrPassiveUnsupported is returned for an empty-query request. Passive
// retrieval is specification, not absence: the session-start policies land with
var ErrPassiveUnsupported = errors.New("passive retrieval is not served by Candidates yet")

const edgeChunkIDs = 200

// Candidates runs retrieval for one request and returns a wider, untrimmed set
// with the scoring facts, leg statuses and edges the assembler needs.
func (s *Store) Candidates(ctx context.Context, req CandidateRequest) (*CandidateSet, error) {
	if err := validateCandidateRequest(req); err != nil {
		return nil, err
	}
	if s.readDB == nil {
		s.warnNoReadHandle()
	}
	handle := s.readHandle()
	if handle == nil {
		return nil, fmt.Errorf("candidates: store has no database handle")
	}

	s.mu.RLock()
	demotion := s.demotionThreshold
	floor := s.vectorMinSimilarity
	s.mu.RUnlock()

	p := req.Params
	if p.RRFK == 0 && p.FTSWeight == 0 && p.VecWeight == 0 {
		p = DefaultSearchParams()
	}
	p.ProjectID = req.ProjectID
	p.MinSimilarity = floor
	p.Scope = req.Scope

	ftsTopK := req.Fetch.FTSTopK
	vecTopK := req.Fetch.VectorTopK
	if ftsTopK <= 0 {
		ftsTopK = req.Fetch.Limit * 2
	}
	if vecTopK <= 0 {
		vecTopK = req.Fetch.Limit * 2
	}

	tx, err := handle.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("candidates: begin read snapshot: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	identity := s.embeddingIdentityValue()
	cand := &Store{
		db:                  s.db,
		snapshot:            tx,
		logger:              s.logger,
		demotionThreshold:   demotion,
		vectorMinSimilarity: floor,
		foreignWarned:       s.foreignWarned,
		scratch:             s.scratch,
		vcache:              s.vcache,
	}
	cand.embeddingIdentity.Store(identityPtr(identity))

	set := &CandidateSet{Legs: map[string]LegStatus{}}
	if req.AsOf != nil {
		return cand.candidatesAsOf(ctx, req, p, ftsTopK, set)
	}
	if req.Query == "" {
		// report, and the abstention floor reads it — a leg that never ran must
		return cand.candidatesPassive(ctx, req, set)
	}
	if req.Explain {
		p.trace = newSearchTrace()
	}

	fts, vec, err := cand.runCandidateLegs(ctx, req, p, ftsTopK, vecTopK, set)
	if err != nil {
		return nil, err
	}
	if err := set.totalLegFailure(); err != nil {
		return nil, err
	}

	fused := fuseCandidatePool(fts.rows, vec.rows, p)
	var preScope []string
	if req.Explain {
		preScope = poolIDsOf(fused)
	}
	pool := scopeEligiblePool(fused, p)
	if len(pool) == 0 {
		set.EdgesStatus = EdgeStatus{Status: edgesUnavailable}
		if req.Explain {
			if err := cand.fillExplain(ctx, set, req, p, preScope); err != nil {
				return nil, err
			}
		}
		return set, nil
	}

	scores := make(map[string]float64, len(pool))
	for _, c := range pool {
		scores[c.id] = c.score
	}

	window := selectWindow(pool, req.Fetch.Limit, p)
	beforeHybridHydrate(window.IDs)
	hydrated, err := hydrateWindow(ctx, cand, window, pool)
	if err != nil {
		return nil, err
	}
	selected := selectHydratedWindow(hydrated, window, poolIDsOf(pool))
	selected = decayRank(selected, scores, p, req.Fetch.Limit, req.Now)
	selected = cand.demoteSuperseded(ctx, selected, p)
	kept, removed, lostTo, standIns := cand.dropNearDuplicates(ctx, selected, p, req.Now)
	selected = kept
	droppedIDs := make([]string, len(removed))
	for i, m := range removed {
		droppedIDs[i] = m.ID
	}

	tail, err := cand.hydrateTail(ctx, pool, selected, droppedIDs, scores, req.Fetch.Limit, req.Now)
	if err != nil {
		return nil, err
	}

	// The survivors are partitioned by MEMBERSHIP, never by index: position
	if len(tail) > 0 {
		combined := append(append([]Memory{}, selected...), tail...)
		keptAll, passLosers, lostToPass, standInsPass := cand.dropNearDuplicatesOver(ctx, combined, len(selected), p, req.Now)
		if len(passLosers) > 0 {
			inWindow := make(map[string]bool, len(selected))
			for _, m := range selected {
				inWindow[m.ID] = true
			}
			keptWindow := make([]Memory, 0, len(selected))
			keptTail := make([]Memory, 0, len(tail))
			for _, m := range keptAll {
				if inWindow[m.ID] {
					keptWindow = append(keptWindow, m)
				} else {
					keptTail = append(keptTail, m)
				}
			}
			selected, tail = keptWindow, keptTail
			for _, m := range passLosers {
				removed = append(removed, m)
				droppedIDs = append(droppedIDs, m.ID)
				lostTo[m.ID] = lostToPass[m.ID]
				if ids := standInsPass[m.ID]; len(ids) > 0 {
					if standIns == nil {
						standIns = make(map[string][]string)
					}
					standIns[m.ID] = ids
				}
			}
		}
	}

	rows := make([]Candidate, 0, len(selected)+len(tail))
	for _, m := range append(selected, tail...) {
		c := candidateOf(m, scores, fts, vec, req.Now)
		c.KeywordReserved = window.Reserved[m.ID]
		rows = append(rows, c)
	}
	set.Rows = rows
	set.Widened = len(rows) > req.Fetch.Limit

	// read over a window it never saw. Filing them here, before the explain
	for _, m := range removed {
		c := candidateOf(m, scores, fts, vec, req.Now)
		c.KeywordReserved = window.Reserved[m.ID]
		set.DroppedLosers = append(set.DroppedLosers, DroppedLoser{
			Candidate: c,
			LostTo:    lostTo[m.ID],
			StandIns:  standIns[m.ID],
		})
	}

	if req.Explain {
		if err := cand.fillExplain(ctx, set, req, p, preScope); err != nil {
			return nil, err
		}
	}

	scope := edgeScopeIDs(rows, req.Fetch.Limit)
	edges, status := cand.loadCandidateEdges(ctx, scope)
	set.Edges, set.EdgesStatus = edges, status

	// so the retriever must not rearrange it; the assembler moves a replacement
	if status.Status != edgesErr {
		scoped := rows[:len(scope)]
		if repl := lexicalReplacements(scoped, edges, req.Now); len(repl) > 0 {
			for i := range scoped {
				scoped[i].MaybeReplacedBy = repl[scoped[i].ID]
			}
		}
	}
	if named := cand.querySupersededBy(ctx, rows[:len(scope)]); len(named) > 0 {
		for i := range scope {
			rows[i].SupersededBy = named[rows[i].ID]
		}
	}

	// about a memory, which is the one thing this read must never produce.
	counts, err := evidenceCountsFor(ctx, cand.queryDB(), scope)
	if err != nil {
		return nil, fmt.Errorf("candidates: evidence counts: %w", err)
	}
	for i := range rows {
		rows[i].Evidence = counts[rows[i].ID]
	}

	// A row the vector leg could never have retrieved — no embedding it could
	if vecStatus := set.Legs["vector"]; vecStatus.Applicable && vecStatus.Available {
		judged := rows
		if len(set.DroppedLosers) > 0 {
			judged = make([]Candidate, 0, len(rows)+len(set.DroppedLosers))
			judged = append(judged, rows...)
			for _, l := range set.DroppedLosers {
				judged = append(judged, l.Candidate)
			}
		}
		unembedded, err := unembeddedAmong(ctx, cand.queryDB(), judged, identity, len(req.QueryVec))
		if err != nil {
			return nil, fmt.Errorf("candidates: unembedded rows: %w", err)
		}
		for i := range rows {
			rows[i].Unembedded = unembedded[rows[i].ID]
		}
		for i := range set.DroppedLosers {
			set.DroppedLosers[i].Unembedded = unembedded[set.DroppedLosers[i].ID]
		}
	}
	return set, nil
}

// it could never have considered.
func unembeddedAmong(ctx context.Context, db Queryer, candidates []Candidate, identity string, width int) (map[string]bool, error) {
	out := make(map[string]bool, len(candidates))
	if len(candidates) == 0 {
		return out, nil
	}
	ids := make([]string, len(candidates))
	for i, c := range candidates {
		ids[i] = c.ID
	}
	head := `SELECT m.id FROM memories m LEFT JOIN memory_embeddings e ON e.memory_id = m.id`
	var prefix []any
	if identity != "" {
		head += ` AND e.model = ?`
		prefix = append(prefix, identity)
	}
	if width > 0 {
		head += ` AND length(e.embedding)/4 = ?`
		prefix = append(prefix, width)
	}
	for start := 0; start < len(ids); start += edgeChunkIDs {
		end := min(start+edgeChunkIDs, len(ids))
		chunk := ids[start:end]
		ph := make([]string, len(chunk))
		args := make([]any, 0, len(prefix)+len(chunk))
		args = append(args, prefix...)
		for i, id := range chunk {
			ph[i] = "?"
			args = append(args, id)
		}
		rows, err := db.QueryContext(ctx, head+
			` WHERE m.id IN (`+strings.Join(ph, ",")+`) AND e.memory_id IS NULL`, args...)
		if err != nil {
			return nil, fmt.Errorf("read unembedded rows: %w", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close() //nolint:errcheck
				return nil, fmt.Errorf("scan unembedded rows: %w", err)
			}
			out[id] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close() //nolint:errcheck
			return nil, fmt.Errorf("iterate unembedded rows: %w", err)
		}
		rows.Close() //nolint:errcheck
	}
	return out, nil
}

func (c *CandidateSet) totalLegFailure() error {
	attempted, failed := 0, 0
	var causes []string
	for _, name := range []string{"fts", "vector"} {
		leg := c.Legs[name]
		if !leg.Attempted {
			continue
		}
		attempted++
		if !leg.Available {
			failed++
			causes = append(causes, name+" leg: "+leg.Err)
		}
	}
	if attempted == 0 || failed < attempted {
		return nil
	}
	return fmt.Errorf("candidates: every applicable retrieval leg failed (%s)", strings.Join(causes, "; "))
}

func validateCandidateRequest(req CandidateRequest) error {
	if req.Now.IsZero() {
		return errors.New("candidates: Now is required: the candidate path does not read the wall clock")
	}
	switch req.Mode {
	case ProjectScoped, GlobalOnly, AllProjects:
	default:
		return fmt.Errorf("candidates: unknown project mode %q", req.Mode)
	}
	switch req.Condition {
	case CondHybrid, CondFTSOnly, CondVectorOnly:
	default:
		return fmt.Errorf("candidates: unknown condition %q", req.Condition)
	}
	if req.Condition == CondVectorOnly && len(req.QueryVec) == 0 {
		return errors.New("candidates: vector-only retrieval requires a query vector")
	}
	if req.AsOf != nil {
		if req.AsOf.IsZero() {
			return errors.New("candidates: AsOf is required to name an instant; the zero time is not one")
		}
		if req.Explain {
			// scoring path never saw. The assembler refuses the combination
			return errors.New("candidates: explain is a projection of the current ranking and cannot describe a " +
				"historical (as_of) read, whose versions were never ranked — ask for one or the other")
		}
		if req.Query == "" {
			return errors.New("candidates: a passive request cannot also be a historical (as_of) read: the passive " +
				"policies select over live rows and a historical read selects over recorded versions — use one or the other")
		}
		if req.Condition == CondVectorOnly {
			// between were never computed. Answering with the keyword leg instead
			return errors.New("candidates: vector-only retrieval is not available for a historical (as_of) read: " +
				"embeddings record current content only, so a past content set has no vectors — ask for hybrid or fts_only")
		}
	}
	if len(req.Passive) > 0 && req.Query != "" {
		// path, which never reads Passive — so the per-bucket over-fetches would be
		return errors.New("candidates: a request carrying passive policies must have an empty query: a query is answered by the " +
			"fusion path, which reads no policy and sizes its own window — a passive request is the shape that uses them")
	}
	if req.Query == "" {
		if req.Explain {
			// that the retrieval never computed.
			return errors.New("candidates: explain requires a query: a passive retrieval scores no candidate, so there is no ranking to explain")
		}
		return validatePassivePolicies(req.Passive)
	}
	if req.Fetch.Limit <= 0 {
		return fmt.Errorf("candidates: fetch limit must be positive, got %d", req.Fetch.Limit)
	}
	return nil
}

type ftsLeg struct {
	rows  []Memory
	index map[string]candidateLegFact
}

type vecLeg struct {
	rows  []ScoredMemory
	index map[string]candidateLegFact
}

type candidateLegFact struct {
	rank  int
	score float64
}

func (s *Store) runCandidateLegs(ctx context.Context, req CandidateRequest, p SearchParams, ftsTopK, vecTopK int, set *CandidateSet) (ftsLeg, vecLeg, error) {
	project := req.ProjectID
	if req.Mode == GlobalOnly {
		project = "_global"
	}
	var out ftsLeg
	ftsStatus := LegStatus{Applicable: req.Condition == CondHybrid || req.Condition == CondFTSOnly}
	if ftsStatus.Applicable {
		var rows []Memory
		var err error
		if req.Mode == AllProjects {
			rows, err = s.searchFTSValidating(ctx, "", req.Query, ftsTopK, req.Now, true)
		} else {
			rows, err = s.searchFTSValidating(ctx, project, req.Query, ftsTopK, req.Now, false)
		}
		ftsStatus.Attempted = true
		ftsStatus.Available = err == nil
		ftsStatus.Eligible = len(rows)
		ftsStatus.Truncated = err == nil && len(rows) >= ftsTopK
		if err != nil {
			ftsStatus.Err = err.Error()
		} else {
			out.index = make(map[string]candidateLegFact, len(rows))
			for i, m := range rows {
				if _, seen := out.index[m.ID]; !seen {
					out.index[m.ID] = candidateLegFact{rank: i}
				}
			}
			out.rows = rows
		}
		ftsStatus.CoverageComplete = ftsStatus.Available && !ftsStatus.Truncated
	}
	set.Legs["fts"] = ftsStatus

	if ftsStatus.Applicable && ftsStatus.Available && len(out.rows) == 0 {
		n, err := s.keywordValidityExcluded(ctx, req, project, req.Mode == AllProjects)
		if err != nil {
			return out, vecLeg{}, err
		}
		set.ValidityExcluded += n
	}

	var vec vecLeg
	vecStatus := LegStatus{Applicable: req.Condition == CondHybrid || req.Condition == CondVectorOnly}
	if vecStatus.Applicable && len(req.QueryVec) > 0 {
		vecStatus.Attempted = true
		var facts vectorLegFacts
		var raw []ScoredMemory
		var err error
		if req.Mode == AllProjects {
			raw, facts, err = s.searchVectorAllLegAt(ctx, req.QueryVec, vecTopK, req.Now)
		} else {
			raw, facts, err = s.searchVectorLegAt(ctx, project, req.QueryVec, vecTopK, nil, req.Now)
		}
		vecStatus.Available = err == nil
		if err != nil {
			vecStatus.Err = err.Error()
		} else {
			kept := filterVectorFloor(raw, p.MinSimilarity)
			vecStatus.Truncated = len(raw) >= vecTopK
			vecStatus.DimMismatch = facts.mismatched + facts.foreign
			// never attempted, which the outcome line prints as `not_run`, so
			if facts.usable == 0 && vecStatus.DimMismatch > 0 {
				vecStatus.Attempted = false
				vecStatus.Available = false
			}
			vecStatus.Eligible = len(kept)
			if len(raw) == 0 {
				set.ValidityExcluded += facts.validityExcluded
			}
			// usable" says nothing about the rows I never saw. A leg that cannot
			vecStatus.CoverageComplete = false
			vec.index = make(map[string]candidateLegFact, len(kept))
			for i, sm := range kept {
				vec.index[sm.MemoryID] = candidateLegFact{rank: i, score: float64(sm.Score)}
			}
			vec.rows = kept
			if req.Explain {
				// side that the keyword leg never retrieved is out of the
				stampVectorFloor(p.trace, raw, kept, p.ProjectID)
				if len(kept) < len(raw) {
					keptSet := make(map[string]bool, len(kept))
					for _, v := range kept {
						keptSet[v.MemoryID] = true
					}
					for _, v := range raw {
						if keptSet[v.MemoryID] {
							continue
						}
						if _, inFTS := out.index[v.MemoryID]; inFTS {
							continue
						}
						set.floorDroppedIDs = append(set.floorDroppedIDs, v.MemoryID)
					}
				}
			}
		}
	}
	set.Legs["vector"] = vecStatus
	return out, vec, nil
}

// describe rows the leg never considered.
func (s *Store) keywordValidityExcluded(ctx context.Context, req CandidateRequest, project string, allProjects bool) (int, error) {
	query, args := keywordValidityExcludedStatement(project, sanitizeFTS(req.Query, s.logger), req.Now, allProjects)
	var n int
	if err := s.queryDB().QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("candidates: keyword validity exclusion count: %w", err)
	}
	return n, nil
}

func poolIDsOf(pool []*hybridCandidate) []string {
	ids := make([]string, len(pool))
	for i, c := range pool {
		ids[i] = c.id
	}
	return ids
}

// payload must describe the same database state as the search it explains.
func (s *Store) fillExplain(ctx context.Context, set *CandidateSet, req CandidateRequest, p SearchParams, preScope []string) error {
	// then the eligible rows the window and its tail never reached. The
	inRows := make(map[string]bool, len(set.Rows))
	for _, c := range set.Rows {
		inRows[c.ID] = true
	}
	excludedIDs := make([]string, 0, len(preScope))
	for _, id := range preScope {
		if !inRows[id] {
			excludedIDs = append(excludedIDs, id)
		}
	}
	excluded, err := s.GetByIDs(ctx, excludedIDs)
	if err != nil {
		return fmt.Errorf("candidates: hydrate explain exclusions: %w", err)
	}
	set.Excluded = orderByIDs(excluded, excludedIDs)

	dropped, err := s.GetByIDs(ctx, set.floorDroppedIDs)
	if err != nil {
		return fmt.Errorf("candidates: hydrate explain floor drops: %w", err)
	}
	set.FloorDropped = orderByIDs(dropped, set.floorDroppedIDs)

	// decayRank orders only the window, so a row it never ordered carries no
	for _, c := range set.Rows {
		if t := p.trace.row(c.ID); t != nil && t.Decay == 0 {
			t.Decay, t.AgeDays = c.Decay, c.AgeDays
		}
	}
	for _, bucket := range [][]Memory{set.Excluded, set.FloorDropped} {
		for _, m := range bucket {
			if t := p.trace.row(m.ID); t != nil && t.Decay == 0 {
				t.AgeDays = ageDays(m.CreatedAt, req.Now)
				t.Decay = DecayFactor(m.Category, m.Content, m.Retention, m.Pinned, t.AgeDays)
			}
		}
	}

	set.RankFacts = p.trace.rows
	set.ExplainKnobs = ExplainKnobs{
		RRFK:        p.RRFK,
		FTSWeight:   p.FTSWeight,
		VecWeight:   p.VecWeight,
		VectorFloor: float64(p.MinSimilarity),
	}
	return nil
}

// holds is absent from the result, exactly as if it had never been a
func orderByIDs(hydrated []Memory, ids []string) []Memory {
	byID := make(map[string]Memory, len(hydrated))
	for _, m := range hydrated {
		byID[m.ID] = m
	}
	out := make([]Memory, 0, len(ids))
	for _, id := range ids {
		if m, ok := byID[id]; ok {
			out = append(out, m)
		}
	}
	return out
}

// matches. A retrieval failure and an empty result must not look alike.
func hydrateWindow(ctx context.Context, s *Store, window HybridWindow, pool []*hybridCandidate) ([]Memory, error) {
	if len(window.IDs) == 0 {
		return nil, nil
	}
	hydrated, err := s.GetByIDs(ctx, window.IDs)
	if err != nil {
		return nil, fmt.Errorf("candidates: hydrate window: %w", err)
	}
	if len(hydrated) == len(window.IDs) {
		return selectHydratedWindow(hydrated, window, nil), nil
	}
	poolIDs := poolIDsOf(pool)
	backfill, err := s.GetByIDs(ctx, poolIDs)
	if err != nil {
		return nil, fmt.Errorf("candidates: hydrate window backfill: %w", err)
	}
	return selectHydratedWindow(backfill, window, poolIDs), nil
}

func (s *Store) hydrateTail(ctx context.Context, pool []*hybridCandidate, selected []Memory, dropped []string, scores map[string]float64, limit int, now time.Time) ([]Memory, error) {
	taken := make(map[string]bool, len(selected)+len(dropped))
	for _, m := range selected {
		taken[m.ID] = true
	}
	for _, id := range dropped {
		taken[id] = true
	}
	ids := make([]string, 0, limit)
	for _, c := range pool {
		if len(ids) == limit {
			break
		}
		if !taken[c.id] {
			taken[c.id] = true
			ids = append(ids, c.id)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.GetByIDs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("candidates: hydrate tail: %w", err)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		si, sj := tailScore(rows[i], scores, now), tailScore(rows[j], scores, now)
		if si != sj {
			return si > sj
		}
		if scores[rows[i].ID] != scores[rows[j].ID] {
			return scores[rows[i].ID] > scores[rows[j].ID]
		}
		return rows[i].ID < rows[j].ID
	})
	return rows, nil
}

func tailScore(m Memory, scores map[string]float64, now time.Time) float64 {
	return scores[m.ID] * DecayFactor(m.Category, m.Content, m.Retention, m.Pinned, ageDays(m.CreatedAt, now))
}

func candidateOf(m Memory, scores map[string]float64, fts ftsLeg, vec vecLeg, now time.Time) Candidate {
	c := Candidate{Memory: m}
	c.Base = scores[m.ID]
	c.AgeDays = ageDays(m.CreatedAt, now)
	c.Decay = DecayFactor(m.Category, m.Content, m.Retention, m.Pinned, c.AgeDays)
	c.Score = c.Base * c.Decay
	c.FTSRank, c.VectorRank, c.VectorScore = -1, -1, -1
	if f, ok := fts.index[m.ID]; ok {
		c.FTSRank = f.rank
	}
	if f, ok := vec.index[m.ID]; ok {
		c.VectorRank = f.rank
		c.VectorScore = f.score
	}
	return c
}

func edgeScopeIDs(rows []Candidate, window int) []string {
	limit := len(rows)
	if window > 0 && limit > window*2 {
		limit = window * 2
	}
	ids := make([]string, limit)
	for i := range limit {
		ids[i] = rows[i].ID
	}
	return ids
}

func (s *Store) loadCandidateEdges(ctx context.Context, ids []string) ([]LinkEdge, EdgeStatus) {
	if len(ids) < 2 {
		return nil, EdgeStatus{Status: edgesUnavailable}
	}
	var edges []LinkEdge
	chunks := 0
	for start := 0; start < len(ids); start += edgeChunkIDs {
		end := min(start+edgeChunkIDs, len(ids))
		chunks++
		chunk, err := s.edgesWithin(ctx, ids[start:end])
		if err != nil {
			s.logger.Debug("candidate edge load failed", "error", err)
			return nil, EdgeStatus{Status: edgesErr, Err: err.Error(), Chunks: chunks}
		}
		edges = append(edges, chunk...)
	}
	if len(edges) == 0 {
		return nil, EdgeStatus{Status: edgesUnavailable, Chunks: chunks}
	}
	return edges, EdgeStatus{Status: edgesOK, Chunks: chunks}
}

func (s *Store) edgesWithin(ctx context.Context, ids []string) ([]LinkEdge, error) {
	ph := make([]string, len(ids))
	args := make([]any, 0, len(ids)*2)
	for pass := range 2 {
		for i, id := range ids {
			if pass == 0 {
				ph[i] = "?"
			}
			args = append(args, id)
		}
	}
	list := strings.Join(ph, ",")
	rows, err := s.queryDB().QueryContext(ctx, fmt.Sprintf(`
		SELECT source_id, target_id, relation, strength FROM memory_links
		WHERE invalidated_at IS NULL
		  AND source_id IN (%s) AND target_id IN (%s)
	`, list, list), args...)
	if err != nil {
		return nil, fmt.Errorf("candidate edges: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var edges []LinkEdge
	for rows.Next() {
		var e LinkEdge
		var strength float64
		if err := rows.Scan(&e.From, &e.To, &e.Relation, &strength); err != nil {
			return nil, fmt.Errorf("candidate edges: %w", err)
		}
		e.Strength = strength
		edges = append(edges, e)
	}
	return edges, rows.Err()
}
