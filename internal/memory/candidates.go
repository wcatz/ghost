package memory

// Retriever contract (see docs/superpowers/specs/2026-09-25-context-assembler-design.md,
// Decision 1). The DTOs live here rather than in internal/assemble because the
// method is implemented by *Store, and a method cannot name a type from a
// package that imports it. The dependency is one-way: internal/assemble imports
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
// them, and a cross-project search has no project predicate at all.
type ProjectMode string

const (
	ProjectScoped ProjectMode = "scoped"       // project_id = ? OR '_global'
	GlobalOnly    ProjectMode = "global_only"  // project_id = '_global'
	AllProjects   ProjectMode = "all_projects" // no project predicate
)

// Condition selects which retrieval legs run. It is part of the retriever
// contract rather than a SearchParams field because a nil query vector already
// means the vector leg is not attempted, which is a different statement from
// asking for vector-only retrieval.
type Condition string

const (
	CondHybrid     Condition = "hybrid"      // both legs, fused
	CondFTSOnly    Condition = "fts_only"    // keyword leg alone
	CondVectorOnly Condition = "vector_only" // vector leg alone; requires a query vector
)

// CandidateRequest is one retrieval: what to fetch, from where, and the clock
// every time-dependent decision is made against. Now is required and is used
// end to end — the candidate path never reads the wall clock, so a ranking and
// the trace describing it cannot disagree about what time it was.
type CandidateRequest struct {
	ProjectID string
	Mode      ProjectMode
	Query     string
	QueryVec  []float32
	Scope     map[string]string
	Category  string
	Condition Condition
	// Params is the caller's unresolved fusion parameters. The Store applies
	// its configured vector floor to them here rather than trusting them, so a
	// caller cannot bypass search.min_similarity.
	Params SearchParams
	Now    time.Time
	// Fetch is the retrieval depth. Limit is the window: the rows that rank
	// into the answer before any predicate is applied. The returned set is
	// wider than that, so a predicate can be evaluated over rows the window
	// would have cut.
	Fetch Fetch
	// Passive carries the selection policies for an empty query. Populated
	// only for passive retrieval, which arrives with the session-start
	// migration; Candidates rejects a passive request rather than serving an
	// empty set that would read as an empty store.
	Passive []SlicePolicy
}

// Fetch is the retrieval depth. FTSTopK and VectorTopK are the per-leg depths
// (production fetches twice the window from each leg); Limit is the window
// itself, not a trim applied to the result.
type Fetch struct {
	FTSTopK, VectorTopK int
	Limit               int // Candidates returns a wider untrimmed set
}

// SlicePolicy is one passive bucket's selection rules: how wide to fetch, in
// what order, which categories are behavioural, and whether a near-duplicate
// loser is dropped rather than demoted. The buckets differ by policy, not by
// accident, so each carries its own.
type SlicePolicy struct {
	Bucket             string
	Order              string // decay or pinned_importance_updated
	TwoPass            bool
	BehaviorFloor      int
	BehaviorCategories []string
	CategoryWeights    map[string]float64
	CategoryCaps       map[string]int
	OverFetch          int
	DemotionThreshold  float64
	ExcludeSeen        bool
	DropDemotedLosers  bool
}

// CandidateSet is one retrieval's rows plus the facts about how they were
// retrieved. Widened reports that the set is larger than the requested window,
// which is the assembler's evidence that it could filter before closing.
type CandidateSet struct {
	Rows        []Candidate
	Edges       []LinkEdge
	EdgesStatus EdgeStatus
	// Legs is keyed by leg name: "fts" and "vector".
	Legs    map[string]LegStatus
	Widened bool
}

// Candidate is one hydrated row with the scoring facts fusion produced for it.
// The facts are carried rather than re-derived, so a trace cannot report a
// different number from the one that ranked the row.
type Candidate struct {
	Memory
	// FTSRank and VectorRank are 0-based within their leg, and -1 when that
	// leg did not retrieve the row: rank 0 is a real first place, and an
	// absent leg must not look like one.
	FTSRank, VectorRank int
	VectorScore         float64
	// Base is the fused score the window was cut on, after status demotion.
	// Decay is the category-and-age multiplier, and Score is the product the
	// decay order ranked on. Supersede and near-duplicate demotion is a
	// reordering, not a score, so it is expressed by the returned order.
	Base, Decay, Score float64
	AgeDays            float64
}

// LinkEdge is one edge between two candidates. Stages 5 and 6 read it to
// reorder and record conflicts; the assembler never queries for it itself.
type LinkEdge struct {
	From, To, Relation string
	Strength           float64
}

// LegStatus is what one retrieval leg did. CoverageComplete is the reason an
// empty answer may claim absence: a leg that skipped unembedded or
// dimension-mismatched rows, or that returned exactly as many rows as it was
// asked for, cannot support that claim.
type LegStatus struct {
	Attempted, Available bool
	Err                  string
	Truncated            bool
	Applicable           bool
	// Expected is how many rows the leg could have seen, Indexed how many of
	// them carry an embedding, and Unembedded how many carry none. DimMismatch
	// counts the rows the vector leg could not compare against the query
	// vector, whether the width differs or the vector belongs to another
	// recorded space. Eligible counts what the leg returned.
	Expected, Indexed, Eligible, DimMismatch, Unembedded int
	CoverageComplete                                     bool
}

// EdgeStatus is the edge lookup's own outcome, kept separate from the rows so a
// failed lookup can never read as "no edges exist": "ok" (the query ran, and
// may have returned none), "unavailable" (a successful query returned no
// edges) or "err" (the query failed).
type EdgeStatus struct{ Status, Err string }

// edge statuses, as reported in EdgeStatus.Status.
const (
	edgesOK          = "ok"
	edgesUnavailable = "unavailable"
	edgesErr         = "err"
)

// ErrPassiveUnsupported is returned for an empty-query request. Passive
// retrieval is specification, not absence: the session-start policies land with
// the migration that moves that surface onto this seam, and until then an
// honest error beats an empty result set.
var ErrPassiveUnsupported = errors.New("passive retrieval is not served by Candidates yet")

// edgeChunkIDs bounds the placeholder list in one edge query. Both endpoints of
// an edge are named, so a candidate set of n ids needs 2n placeholders, and
// SQLite caps a statement's variables well below what a wide window would need.
const edgeChunkIDs = 200

// Candidates runs retrieval for one request and returns a wider, untrimmed set
// with the scoring facts, leg statuses and edges the assembler needs.
//
// What "untrimmed" means here is deliberate. The window is selected exactly as
// production search selects it — fusion, status demotion, the keyword
// reservation, hydration with its deleted-row backfill, decay over the window,
// then the supersede and near-duplicate demotions over the window — and the
// rows that selection discarded are returned after it, in the same decay order.
// So with no predicate, closing the set to Fetch.Limit reproduces the
// production search exactly, and a predicate can still reach rows the window
// cut. Re-deriving the window's own decisions from the wider pool instead
// would drop the reservation and change the demotions' scope, so it is not done.
//
// Everything runs in one read transaction on the store's read handle: the legs,
// hydration, the edge load and the penalty lookups have to describe the same
// database state, and a concurrent save must not be able to delete a row
// between the leg that selected it and the read that hydrates it.
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

	p := req.Params
	if p.RRFK == 0 && p.FTSWeight == 0 && p.VecWeight == 0 {
		// A caller that passes no parameters gets production's, resolved here
		// rather than at the call site so the floor below is the only one that
		// can apply.
		p = DefaultSearchParams()
	}
	p.ProjectID = req.ProjectID
	p.MinSimilarity = s.vectorMinSimilarityFloor()
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

	s.mu.RLock()
	demotion := s.demotionThreshold
	floor := s.vectorMinSimilarity
	identity := s.embeddingIdentity
	s.mu.RUnlock()
	// A store bound to the transaction: every read below has to come from the
	// snapshot, and the primary handle's single connection is held by the
	// transaction itself.
	//
	// Every knob that steers retrieval is carried across, or this store would
	// run a different search than the one it is standing in for: without the
	// embedding identity the vector leg here scores the foreign-space vectors
	// the real search excludes, and without the floor or the demotion threshold
	// the rows it returns are ones no caller could reproduce.
	cand := &Store{
		db:                  s.db,
		snapshot:            tx,
		logger:              s.logger,
		demotionThreshold:   demotion,
		vectorMinSimilarity: floor,
		embeddingIdentity:   identity,
	}

	set := &CandidateSet{Legs: map[string]LegStatus{}}
	fts, vec := cand.runCandidateLegs(ctx, req, p, ftsTopK, vecTopK, set)

	pool := scopeEligiblePool(fuseCandidatePool(fts.rows, vec.rows, p), p)
	if len(pool) == 0 {
		set.EdgesStatus = EdgeStatus{Status: edgesUnavailable}
		return set, nil
	}

	scores := make(map[string]float64, len(pool))
	for _, c := range pool {
		scores[c.id] = c.score
	}

	window := selectWindow(pool, req.Fetch.Limit, p)
	// The tail is the pool the window cut, capped at the window width: no
	// closure the caller can express admits more rows than the window it was
	// given, so a longer tail could never reach the answer.
	selected := selectHydratedWindow(hydrateWindow(ctx, cand, window, pool), window, poolIDsOf(pool))
	selected = decayRank(selected, scores, p, req.Fetch.Limit, req.Now)
	selected = cand.demoteResults(ctx, selected, p)

	tail, err := cand.hydrateTail(ctx, pool, selected, scores, req.Fetch.Limit, req.Now)
	if err != nil {
		return nil, err
	}

	rows := make([]Candidate, 0, len(selected)+len(tail))
	for _, m := range append(selected, tail...) {
		rows = append(rows, candidateOf(m, scores, fts, vec, req.Now))
	}
	set.Rows = rows
	set.Widened = len(rows) > req.Fetch.Limit

	edges, status := cand.loadCandidateEdges(ctx, edgeScopeIDs(rows, req.Fetch.Limit))
	set.Edges, set.EdgesStatus = edges, status
	return set, nil
}

// validateCandidateRequest rejects a request the store cannot serve honestly,
// before any query runs. It is the store's own half of the contract: Run
// validates the same conditions, and Candidates is reachable directly by the
// bench harness and by tests.
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
	if req.Fetch.Limit <= 0 {
		return fmt.Errorf("candidates: fetch limit must be positive, got %d", req.Fetch.Limit)
	}
	if req.Query == "" {
		return ErrPassiveUnsupported
	}
	return nil
}

// ftsLeg and vecLeg are the two legs' output plus the per-leg rank and score
// indexes the candidates need, so a fact about a row does not cost a second
// lookup.
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

// runCandidateLegs executes the legs the condition selects, recording each
// leg's status. A leg that fails is recorded, not returned: a keyword leg that
// errored while the vector leg returned rows is a partial retrieval, and the
// assembler reports that rather than calling it an empty store.
func (s *Store) runCandidateLegs(ctx context.Context, req CandidateRequest, p SearchParams, ftsTopK, vecTopK int, set *CandidateSet) (ftsLeg, vecLeg) {
	project := req.ProjectID
	if req.Mode == GlobalOnly {
		// The legs filter on `project_id = ? OR project_id = '_global'`, so a
		// global-only request names _global explicitly instead of relying on an
		// empty id matching nothing: every mode then issues the same statement,
		// and no row can be pulled in by an empty project id.
		project = "_global"
	}
	var out ftsLeg
	ftsStatus := LegStatus{Applicable: req.Condition == CondHybrid || req.Condition == CondFTSOnly}
	if ftsStatus.Applicable {
		var rows []Memory
		var err error
		if req.Mode == AllProjects {
			rows, err = s.SearchFTSAll(ctx, req.Query, ftsTopK)
		} else {
			rows, err = s.SearchFTS(ctx, project, req.Query, ftsTopK)
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
				// The first hit wins a repeated id, matching how explain
				// indexes the same leg.
				if _, seen := out.index[m.ID]; !seen {
					out.index[m.ID] = candidateLegFact{rank: i}
				}
			}
			out.rows = rows
		}
		// A keyword leg sees every row in scope, so it is complete coverage
		// exactly when it ran and was not cut short.
		ftsStatus.CoverageComplete = ftsStatus.Available && !ftsStatus.Truncated
	}
	set.Legs["fts"] = ftsStatus

	var vec vecLeg
	vecStatus := LegStatus{Applicable: req.Condition == CondHybrid || req.Condition == CondVectorOnly}
	if vecStatus.Applicable && len(req.QueryVec) > 0 {
		vecStatus.Attempted = true
		var facts vectorLegFacts
		var raw []ScoredMemory
		var err error
		if req.Mode == AllProjects {
			raw, facts, err = s.searchVectorAllLeg(ctx, req.QueryVec, vecTopK)
		} else {
			raw, facts, err = s.searchVectorLeg(ctx, project, req.QueryVec, vecTopK, nil)
		}
		vecStatus.Available = err == nil
		if err != nil {
			vecStatus.Err = err.Error()
		} else {
			kept := filterVectorFloor(raw, p.MinSimilarity)
			vecStatus.Truncated = len(raw) >= vecTopK
			// DimMismatch counts the rows the leg could not compare against the
			// query vector: a different width, or a recorded identity from
			// another vector space (a model or task-prefix change). Both leave
			// the leg silent about that row, which is what coverage has to
			// account for — and after a model change it is every row.
			vecStatus.DimMismatch = facts.mismatched + facts.foreign
			// The counts are what make coverage decidable rather than
			// assumed: a leg that could not see every row cannot support a
			// claim that nothing matched.
			vecStatus.Expected, vecStatus.Indexed = s.vectorCoverage(ctx, req)
			vecStatus.Unembedded = vecStatus.Expected - vecStatus.Indexed
			if vecStatus.Unembedded < 0 {
				vecStatus.Unembedded = 0
			}
			vecStatus.Eligible = len(kept)
			vecStatus.CoverageComplete = vecStatus.Available && !vecStatus.Truncated &&
				vecStatus.DimMismatch == 0 && vecStatus.Unembedded == 0
			vec.index = make(map[string]candidateLegFact, len(kept))
			for i, sm := range kept {
				vec.index[sm.MemoryID] = candidateLegFact{rank: i, score: float64(sm.Score)}
			}
			vec.rows = kept
		}
	}
	set.Legs["vector"] = vecStatus
	return out, vec
}

// vectorCoverage counts the rows the vector leg could have seen and how many
// carry an embedding, in one round trip. A skipped row is a row the leg is
// silent about, so both numbers are part of its status.
func (s *Store) vectorCoverage(ctx context.Context, req CandidateRequest) (expected, indexed int) {
	pred, args := coveragePredicate(req)
	// The predicate names its arguments in both subqueries, so they are bound
	// twice.
	bound := make([]any, 0, len(args)*2)
	bound = append(bound, args...)
	bound = append(bound, args...)
	row := s.queryDB().QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM memories WHERE `+pred+`),
			(SELECT COUNT(*) FROM memory_embeddings e JOIN memories m ON m.id = e.memory_id WHERE `+pred+`)
	`, bound...)
	if err := row.Scan(&expected, &indexed); err != nil {
		// An uncountable coverage is not complete coverage.
		return 0, 0
	}
	return expected, indexed
}

// coveragePredicate is the row set one leg can see, as SQL plus its arguments.
// A leg that cannot name its own scope has no coverage claim to make.
func coveragePredicate(req CandidateRequest) (string, []any) {
	switch req.Mode {
	case AllProjects:
		return "1 = 1", nil
	case GlobalOnly:
		return "project_id = '_global'", nil
	default:
		return "(project_id = ? OR project_id = '_global')", []any{req.ProjectID}
	}
}

// poolIDsOf lists the fused pool in rank order, which is the order a window
// that lost rows to a concurrent delete backfills from.
func poolIDsOf(pool []*hybridCandidate) []string {
	ids := make([]string, len(pool))
	for i, c := range pool {
		ids[i] = c.id
	}
	return ids
}

// hydrateWindow materialises the selected window, falling back to the wider
// pool when a window id no longer exists. It is the same two reads production
// search performs, in the same order, for the same reason: the leg results are
// an earlier snapshot, and a row deleted since then has to disappear rather than
// be emitted from stale leg data.
func hydrateWindow(ctx context.Context, s *Store, window HybridWindow, pool []*hybridCandidate) []Memory {
	if len(window.IDs) == 0 {
		return nil
	}
	hydrated, err := s.GetByIDs(ctx, window.IDs)
	if err != nil {
		// A hydration failure is not a silent short window.
		return nil
	}
	if len(hydrated) == len(window.IDs) {
		return selectHydratedWindow(hydrated, window, nil)
	}
	poolIDs := poolIDsOf(pool)
	backfill, err := s.GetByIDs(ctx, poolIDs)
	if err != nil {
		return selectHydratedWindow(hydrated, window, nil)
	}
	return selectHydratedWindow(backfill, window, poolIDs)
}

// hydrateTail returns the rows the window cut, in the same decay order the
// window is returned in, so a predicate that removes a window row is backfilled
// by the row that would have ranked next.
//
// The tail is ordered but not demoted. Supersede and near-duplicate demotion
// are window-scoped reorders that production applies to the rows it returns;
// widening them here would change the order of rows the window already fixed,
// and deciding them over the assembler's final membership is the conflict and
// dedup stages' job.
func (s *Store) hydrateTail(ctx context.Context, pool []*hybridCandidate, selected []Memory, scores map[string]float64, limit int, now time.Time) ([]Memory, error) {
	taken := make(map[string]bool, len(selected))
	for _, m := range selected {
		taken[m.ID] = true
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

// tailScore is the score the tail is ordered on: the same base × category-and-
// age decay the window is ordered on, so a backfilled row ranks as if it had
// been selected.
func tailScore(m Memory, scores map[string]float64, now time.Time) float64 {
	return scores[m.ID] * DecayFactor(m.Category, m.Pinned, ageDays(m.CreatedAt, now))
}

// candidateOf pairs a hydrated row with the facts fusion produced for it.
func candidateOf(m Memory, scores map[string]float64, fts ftsLeg, vec vecLeg, now time.Time) Candidate {
	c := Candidate{Memory: m}
	c.Base = scores[m.ID]
	c.AgeDays = ageDays(m.CreatedAt, now)
	c.Decay = DecayFactor(m.Category, m.Pinned, c.AgeDays)
	c.Score = c.Base * c.Decay
	// -1 marks a leg that did not retrieve the row; rank 0 is a real first
	// place, and a cosine of 0 is a real measurement of "no similarity".
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

// edgeScopeIDs is the id set the edge load covers: the returned rows, bounded by
// the window width. A link to a row beyond that bound cannot affect any
// decision the caller can reach, because no closure admits that many rows.
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

// loadCandidateEdges loads every valid edge whose two endpoints are candidates.
// A failed lookup is reported rather than swallowed: an empty edge set and a
// failed one mean different things to the stages that read it, and only the
// status can tell them apart.
func (s *Store) loadCandidateEdges(ctx context.Context, ids []string) ([]LinkEdge, EdgeStatus) {
	if len(ids) < 2 {
		return nil, EdgeStatus{Status: edgesUnavailable}
	}
	var edges []LinkEdge
	for start := 0; start < len(ids); start += edgeChunkIDs {
		end := min(start+edgeChunkIDs, len(ids))
		chunk, err := s.edgesWithin(ctx, ids[start:end])
		if err != nil {
			s.logger.Debug("candidate edge load failed", "error", err)
			return nil, EdgeStatus{Status: edgesErr, Err: err.Error()}
		}
		edges = append(edges, chunk...)
	}
	if len(edges) == 0 {
		return nil, EdgeStatus{Status: edgesUnavailable}
	}
	return edges, EdgeStatus{Status: edgesOK}
}

// edgesWithin reads the valid edges among a set of candidate ids.
func (s *Store) edgesWithin(ctx context.Context, ids []string) ([]LinkEdge, error) {
	ph := make([]string, len(ids))
	args := make([]any, 0, len(ids)*2)
	// Both endpoint lists name the same ids, so the pair of IN clauses is the
	// same set twice.
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
