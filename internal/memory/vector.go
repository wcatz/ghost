package memory

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

var beforeHybridHydrateFn atomic.Value // func([]string)

func init() {
	beforeHybridHydrateFn.Store(func([]string) {})
}

func beforeHybridHydrate(ids []string) {
	fn, _ := beforeHybridHydrateFn.Load().(func([]string))
	fn(ids)
}

// StoreEmbedding saves an embedding vector for a memory.
func (s *Store) StoreEmbedding(ctx context.Context, memoryID string, vec []float32, model string) error {
	_, err := s.storeEmbedding(ctx, memoryID, vec, model, nil)
	return err
}

// StoreEmbeddingForContent is StoreEmbedding for a vector computed from a text
func (s *Store) StoreEmbeddingForContent(ctx context.Context, memoryID string, vec []float32, model, content string) (bool, error) {
	return s.storeEmbedding(ctx, memoryID, vec, model, &content)
}

func (s *Store) storeEmbedding(ctx context.Context, memoryID string, vec []float32, model string, wantContent *string) (bool, error) {
	blob := float32sToBytes(vec)

	tx, lock, err := s.beginWrite(ctx, "store-embedding")
	if err != nil {
		return false, fmt.Errorf("begin store embedding: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if wantContent != nil {
		var current string
		err := tx.QueryRowContext(ctx, `SELECT content FROM memories WHERE id = ?`, memoryID).Scan(&current)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("check embedded content: %w", err)
		}
		if current != *wantContent {
			return false, nil
		}
	}

	if _, err := tx.ExecContext(ctx, `
		DELETE FROM link_scans
		WHERE memory_id = ? AND EXISTS (
			SELECT 1 FROM memory_embeddings WHERE memory_id = ? AND model <> ?
		)
	`, memoryID, memoryID, model); err != nil {
		return false, fmt.Errorf("invalidate link scan: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO memory_embeddings (memory_id, embedding, model)
		VALUES (?, ?, ?)
		ON CONFLICT(memory_id) DO UPDATE SET embedding = excluded.embedding, model = excluded.model, created_at = datetime('now')
	`, memoryID, blob, model); err != nil {
		return false, fmt.Errorf("store embedding: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit store embedding: %w", err)
	}
	lock.reportHold("store-embedding", time.Now())
	return true, nil
}

// DeleteEmbedding removes the embedding for a memory.
func (s *Store) DeleteEmbedding(ctx context.Context, memoryID string) error {
	_, err := s.execGuardedWrite(ctx, "delete-embedding",
		`DELETE FROM memory_embeddings WHERE memory_id = ?`, memoryID)
	return err
}

// EmbeddingCosines returns the cosine similarity between queryVec and the stored
func (s *Store) EmbeddingCosines(ctx context.Context, ids []string, queryVec []float32) (map[string]float32, error) {
	if len(ids) == 0 || len(queryVec) == 0 {
		return nil, nil
	}
	ph := make([]string, len(ids))
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args[i] = id
	}
	identity := s.configuredEmbeddingIdentity()

	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.queryDB().QueryContext(ctx, fmt.Sprintf(`
		SELECT memory_id, embedding, model FROM memory_embeddings
		WHERE memory_id IN (%s)
	`, strings.Join(ph, ",")), args...)
	if err != nil {
		return nil, fmt.Errorf("embedding cosines: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	cosines := make(map[string]float32, len(ids))
	foreign := make(map[string]int)
	mismatched, mismatchedModel := 0, ""
	for rows.Next() {
		var id, model string
		var blob []byte
		if err := rows.Scan(&id, &blob, &model); err != nil {
			return nil, fmt.Errorf("embedding cosines: %w", err)
		}
		if identity != "" && model != identity {
			foreign[model]++
			continue
		}
		vec := bytesToFloat32s(blob)
		if len(vec) != len(queryVec) {
			mismatched++
			if mismatchedModel == "" {
				mismatchedModel = model
			}
			continue
		}
		cosines[id] = cosineSimilarity(queryVec, vec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("embedding cosines: %w", err)
	}
	if s.logger != nil && len(foreign) > 0 {
		models := make([]string, 0, len(foreign))
		for m := range foreign {
			models = append(models, m)
		}
		sort.Strings(models)
		for _, m := range models {
			if s.warnForeignOnce(m) {
				s.logger.Warn("embedding cosine lookup skipped rows from another vector space — the configured embedding model changed and those rows are waiting to be re-embedded (reported once per retired identity)",
					"skipped", foreign[m], "scored", len(cosines), "configured_identity", identity, "stored_identity", m)
			}
		}
	}
	if s.logger != nil && mismatched > 0 {
		s.logger.Warn("embedding cosine lookup skipped vectors whose width differs from the query — the embedding model likely changed; re-embed to restore comparable scores",
			"skipped", mismatched, "scored", len(cosines), "query_dims", len(queryVec), "stored_identity", mismatchedModel)
	}
	return cosines, nil
}

// UnembeddedMemoryIDs returns memory IDs that still need an embedding produced
func (s *Store) UnembeddedMemoryIDs(ctx context.Context, projectID, identity string, limit int) ([]string, error) {
	query := `
		SELECT m.id
		FROM memories m
		LEFT JOIN memory_embeddings e ON e.memory_id = m.id
		WHERE m.project_id = ? AND e.memory_id IS NULL
		ORDER BY m.created_at DESC
		LIMIT ?
	`
	args := []any{projectID, limit}
	if identity != "" {
		query = `
		SELECT m.id
		FROM memories m
		LEFT JOIN memory_embeddings e ON e.memory_id = m.id
		WHERE m.project_id = ? AND (e.memory_id IS NULL OR e.model <> ?)
		ORDER BY m.created_at DESC
		LIMIT ?
		`
		args = []any{projectID, identity, limit}
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("unembedded memories: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// GetMemoryContent returns the content of a memory by ID.
func (s *Store) GetMemoryContent(ctx context.Context, id string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var content string
	err := s.db.QueryRowContext(ctx, `SELECT content FROM memories WHERE id = ?`, id).Scan(&content)
	return content, err
}

// SearchVector performs brute-force cosine similarity search against stored embeddings.
func (s *Store) SearchVector(ctx context.Context, projectID string, queryVec []float32, limit int) ([]ScoredMemory, error) {
	rows, _, err := s.searchVectorLeg(ctx, projectID, queryVec, limit, nil)
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, err
}

// SearchVectorScoped is SearchVector with a scope constraint applied *before*
func (s *Store) SearchVectorScoped(ctx context.Context, projectID string, queryVec []float32, limit int, scope map[string]string) ([]ScoredMemory, error) {
	rows, _, err := s.searchVectorLeg(ctx, projectID, queryVec, limit, scope)
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, err
}

type vectorLegFacts struct {
	mismatched, foreign, validityExcluded int
	usable                                int
}

func (s *Store) searchVectorLeg(ctx context.Context, projectID string, queryVec []float32, limit int, scope map[string]string) ([]ScoredMemory, vectorLegFacts, error) {
	return s.searchVectorLegAt(ctx, projectID, queryVec, limit, scope, time.Time{})
}

func (s *Store) searchVectorLegAt(ctx context.Context, projectID string, queryVec []float32, limit int, scope map[string]string, now time.Time) ([]ScoredMemory, vectorLegFacts, error) {
	identity := s.configuredEmbeddingIdentity()

	cp, err := s.vectorCorpusFor(ctx, queryVec, identity)
	if err != nil {
		return nil, vectorLegFacts{}, err
	}
	if cp != nil {
		scratch := s.borrowVectorRows()
		defer s.returnVectorRows(scratch)
		v := s.viewVectorCorpus(cp, scratch, projectID, false, queryVec)
		defer releaseViewScratch(scratch, v)
		v.now = now
		v.splitGlobal = true
		return v.search(queryVec, limit, scope), v.facts, nil
	}

	rows := s.borrowVectorRows()
	defer s.returnVectorRows(rows)

	if err := s.snapshotVectors(ctx, vectorProjectScanQuery, []any{projectID}, vectorProjectPredicate, []any{projectID}, queryVec, identity, rows); err != nil {
		return nil, vectorLegFacts{}, err
	}
	rows.now = now
	rows.splitGlobal = true
	return rows.search(queryVec, limit, scope), rows.facts, nil
}

func (s *Store) warnForeignOnce(storedIdentity string) bool {
	if s.foreignWarned == nil {
		return true
	}
	s.foreignWarned.mu.Lock()
	defer s.foreignWarned.mu.Unlock()
	if s.foreignWarned.warned[storedIdentity] {
		return false
	}
	s.foreignWarned.warned[storedIdentity] = true
	return true
}

const minVectorSimilarity float32 = 0

// ScoredMemory pairs a memory ID with a relevance score.
type ScoredMemory struct {
	MemoryID  string
	Score     float32
	Scope     map[string]string
	ProjectID string
	Resolved  bool
}

// SearchParams parameterizes hybrid-search fusion. The zero value disables
type SearchParams struct {
	FTSWeight       float64 // RRF weight of the full-text leg
	VecWeight       float64 // RRF weight of the vector leg
	RRFK            int     // RRF smoothing constant (Cormack & Clarke use 60)
	DecayEnabled    bool
	SupersedeDemote bool
	DecayReselect   bool
	MinSimilarity   float32
	Scope           map[string]string
	ProjectID       string
	trace           *searchTrace
}

// DefaultSearchParams returns the production fusion parameters.
func DefaultSearchParams() SearchParams {
	return SearchParams{
		FTSWeight:       0.3,
		VecWeight:       0.7,
		RRFK:            60,
		DecayEnabled:    true,
		SupersedeDemote: true,
		MinSimilarity:   0, // historical: only non-positive cosines dropped
	}
}

func filterVectorFloor(vec []ScoredMemory, floor float32) []ScoredMemory {
	if floor <= 0 || len(vec) == 0 {
		return vec
	}
	kept := vec[:0:0]
	for _, sm := range vec {
		if sm.Score > floor {
			kept = append(kept, sm)
		}
	}
	return kept
}

func stampVectorFloor(trace *searchTrace, raw, kept []ScoredMemory, searchProjectID string) {
	if trace == nil || len(kept) >= len(raw) {
		return
	}
	keptSet := make(map[string]bool, len(kept))
	for _, v := range kept {
		keptSet[v.MemoryID] = true
	}
	for _, v := range raw {
		if keptSet[v.MemoryID] {
			continue
		}
		if t := trace.row(v.MemoryID); t != nil {
			t.FloorDropped = true
			t.FloorScore = float64(v.Score)
			t.RowProject = v.ProjectID
			t.ProjectMatch = searchProjectID == "" || v.ProjectID == searchProjectID
		}
	}
}

func (s *Store) demoteSuperseded(ctx context.Context, results []Memory, p SearchParams) []Memory {
	if !p.SupersedeDemote || len(results) < 2 {
		return results
	}
	ids := make([]string, len(results))
	protected := make(map[string]bool, len(results))
	for i, m := range results {
		ids[i] = m.ID
		protected[m.ID] = RetentionExempt(m)
	}
	s.mu.RLock()
	penalty, err := supersedeVerdicts(ctx, s.queryDB(), ids, p.trace, protected)
	s.mu.RUnlock()
	if err != nil {
		s.logger.Debug("supersede demote: lookup failed", "error", err)
		return results
	}
	if len(penalty) == 0 {
		return results
	}
	return StableDemote(results, func(m Memory) string { return m.ID }, penalty)
}

func parseCreatedAt(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04:05", s)
	if err != nil {
		return time.Time{}
	}
	return t
}

var datedFactRE = regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)

// ContentCarriesDate reports whether content carries a calendar date. It is the
func ContentCarriesDate(content string) bool {
	return datedFactRE.MatchString(content)
}

// DecayFactor is the time-decay multiplier for a memory of this category, this
func DecayFactor(category, content, retention string, pinned bool, ageDays float64) float64 {
	return decayFactorDated(category, ContentCarriesDate(content), retention, pinned, ageDays)
}

func decayFactorDated(category string, dated bool, retention string, pinned bool, ageDays float64) float64 {
	if pinned {
		return 1.0
	}
	return categoryDecayDated(category, dated, ageDays) * RetentionDecayFactor(retention, pinned, ageDays)
}

func categoryDecayDated(category string, dated bool, ageDays float64) float64 {
	switch category {
	case "preference", "convention":
		return 1.0
	case "fact":
		if !dated {
			return 1.0
		}
		return math.Max(0.3, 1.0/(1.0+ageDays/45.0))
	case "pattern", "architecture":
		return math.Max(0.3, 1.0/(1.0+ageDays/45.0))
	default:
		return math.Max(0.15, 1.0/(1.0+ageDays/30.0))
	}
}

func decayRank(results []Memory, scores map[string]float64, p SearchParams, limit int, now time.Time) []Memory {
	scored := make([]struct {
		m    Memory
		base float64
	}, len(results))
	for i, m := range results {
		base := scores[m.ID]
		if scores == nil {
			base = 1.0 / float64(p.RRFK+i+1)
		}
		scored[i] = struct {
			m    Memory
			base float64
		}{m, base}
		if t := p.trace.row(m.ID); t != nil {
			t.AgeDays = ageDays(m.CreatedAt, now)
			t.Decay = DecayFactor(m.Category, m.Content, m.Retention, m.Pinned, t.AgeDays)
		}
	}

	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].base != scored[j].base {
			return scored[i].base > scored[j].base
		}
		return scored[i].m.ID < scored[j].m.ID
	})

	baseCut := limit
	if p.DecayReselect && p.DecayEnabled {
		baseCut = limit * 2
	}
	if len(scored) > baseCut {
		scored = scored[:baseCut]
	}

	if p.DecayEnabled {
		sort.SliceStable(scored, func(i, j int) bool {
			fi := scored[i].base * DecayFactor(scored[i].m.Category, scored[i].m.Content, scored[i].m.Retention, scored[i].m.Pinned, ageDays(scored[i].m.CreatedAt, now))
			fj := scored[j].base * DecayFactor(scored[j].m.Category, scored[j].m.Content, scored[j].m.Retention, scored[j].m.Pinned, ageDays(scored[j].m.CreatedAt, now))
			if fi != fj {
				return fi > fj
			}
			return scored[i].m.ID < scored[j].m.ID
		})
	}
	if len(scored) > limit {
		scored = scored[:limit]
	}

	out := make([]Memory, len(scored))
	for i, s := range scored {
		out[i] = s.m
	}
	return out
}

func ageDays(createdAt string, now time.Time) float64 {
	ageDays := now.Sub(parseCreatedAt(createdAt)).Hours() / 24.0
	if ageDays < 0 {
		return 0
	}
	return ageDays
}

func (s *Store) fuseAndRank(ctx context.Context, ftsResults []Memory, vecResults []ScoredMemory, limit int, p SearchParams) ([]Memory, error) {
	pool := scopeEligiblePool(fuseCandidatePool(ftsResults, vecResults, p), p)
	window := selectWindow(pool, limit, p)

	poolIDs := make([]string, len(pool))
	if window.Scores == nil {
		window.Scores = make(map[string]float64, len(pool))
	}
	for i, candidate := range pool {
		poolIDs[i] = candidate.id
		if _, ok := window.Scores[candidate.id]; !ok {
			window.Scores[candidate.id] = candidate.score
		}
	}
	if len(poolIDs) == 0 {
		poolIDs = window.IDs
	}

	beforeHybridHydrate(window.IDs)

	hydrated, err := s.GetByIDs(ctx, window.IDs)
	if err != nil {
		return nil, err
	}
	if len(window.IDs) > 0 && len(hydrated) < len(window.IDs) {
		pool, err := s.GetByIDs(ctx, poolIDs)
		if err != nil {
			return nil, err
		}
		hydrated = pool
	}
	memories := selectHydratedWindow(hydrated, window, poolIDs)
	memories = decayRank(memories, window.Scores, p, limit, time.Now().UTC())
	return s.demoteResults(ctx, memories, p), nil
}

func selectHydratedWindow(hydrated []Memory, window HybridWindow, poolIDs []string) []Memory {
	byID := make(map[string]Memory, len(hydrated))
	for _, memory := range hydrated {
		byID[memory.ID] = memory
	}
	selected := make([]Memory, 0, len(window.IDs))
	seen := make(map[string]bool, len(window.IDs))
	for _, id := range window.IDs {
		if memory, ok := byID[id]; ok {
			selected = append(selected, memory)
			seen[id] = true
		}
	}
	if len(selected) == len(window.IDs) {
		return selected
	}
	for _, id := range poolIDs {
		if len(selected) == len(window.IDs) {
			break
		}
		if seen[id] {
			continue
		}
		if memory, ok := byID[id]; ok {
			selected = append(selected, memory)
			seen[id] = true
		}
	}
	return selected
}

// HybridWindow is the result of hybrid fusion and window selection: the memory
type HybridWindow struct {
	IDs      []string
	Scores   map[string]float64
	Reserved map[string]bool
}

func fuseCandidatePool(ftsResults []Memory, vecResults []ScoredMemory, p SearchParams) []*hybridCandidate {
	byID := make(map[string]*hybridCandidate, len(ftsResults)+len(vecResults))
	get := func(id string) *hybridCandidate {
		c, ok := byID[id]
		if !ok {
			c = &hybridCandidate{id: id}
			byID[id] = c
		}
		return c
	}
	for rank, memory := range ftsResults {
		candidate := get(memory.ID)
		candidate.fts = rank + 1
		candidate.scope = memory.Scope
		candidate.projectID = memory.ProjectID
		candidate.resolved = memory.ResolvedAt != nil
		candidate.score += p.FTSWeight / float64(p.RRFK+rank+1)
		if t := p.trace.row(memory.ID); t != nil {
			t.FTSRank = rank
		}
	}
	for rank, scored := range vecResults {
		candidate := get(scored.MemoryID)
		candidate.vec = rank + 1
		if t := p.trace.row(scored.MemoryID); t != nil {
			t.VectorRank = rank
			t.VectorScore = float64(scored.Score)
		}
		if candidate.scope == nil {
			candidate.scope = scored.Scope
		}
		if candidate.projectID == "" {
			candidate.projectID = scored.ProjectID
		}
		candidate.resolved = candidate.resolved || scored.Resolved
		candidate.score += p.VecWeight / float64(p.RRFK+rank+1)
	}

	pool := make([]*hybridCandidate, 0, len(byID))
	for _, candidate := range byID {
		pool = append(pool, candidate)
	}
	demoteStatus(pool, p)

	sort.Slice(pool, func(i, j int) bool {
		if pool[i].score != pool[j].score {
			return pool[i].score > pool[j].score
		}
		return pool[i].id < pool[j].id
	})
	return pool
}

func scopeEligiblePool(pool []*hybridCandidate, p SearchParams) []*hybridCandidate {
	if len(p.Scope) == 0 {
		return pool
	}
	eligible := pool[:0]
	for _, c := range pool {
		matched := ScopeMatches(c.scope, p.Scope)
		if matched {
			eligible = append(eligible, c)
		}
		if t := p.trace.row(c.id); t != nil {
			t.ScopeMatched = matched
		}
	}
	return eligible
}

// FuseAndSelectWindow owns both halves of hybrid retrieval — fusing the
func FuseAndSelectWindow(ftsResults []Memory, vecResults []ScoredMemory, limit int, p SearchParams) HybridWindow {
	return selectWindow(scopeEligiblePool(fuseCandidatePool(ftsResults, vecResults, p), p), limit, p)
}

func selectWindow(pool []*hybridCandidate, limit int, p SearchParams) HybridWindow {
	width := limit
	if p.DecayReselect && p.DecayEnabled {
		width = limit * 2
	}
	if width <= 0 {
		return HybridWindow{}
	}
	pool = append([]*hybridCandidate(nil), pool...)
	var reserved map[string]bool
	if slots := limit / 5; p.FTSWeight > 0 && slots > 0 && len(pool) > width {
		isReserved := func(c *hybridCandidate) bool {
			if statusDemotionFactor(c.resolved, c.projectID, p.ProjectID) < 1 {
				return false
			}
			return c.vec == 0 && c.fts > 0 && c.fts <= slots
		}
		admitted := make(map[string]bool, width)
		for _, c := range pool[:width] {
			admitted[c.id] = true
		}
		for _, c := range pool {
			if !isReserved(c) || admitted[c.id] {
				continue
			}
			for i := width - 1; i >= 0; i-- {
				if !isReserved(pool[i]) {
					if t := p.trace.row(c.id); t != nil {
						t.KeywordReserved = true
						t.TookSlotFrom = pool[i].id
					}
					if t := p.trace.row(pool[i].id); t != nil {
						t.DisplacedBy = c.id
					}
					pool[i] = c
					if reserved == nil {
						reserved = make(map[string]bool)
					}
					reserved[c.id] = true
					break
				}
			}
			admitted[c.id] = true
		}
		sort.Slice(pool[:width], func(i, j int) bool {
			if pool[i].score != pool[j].score {
				return pool[i].score > pool[j].score
			}
			return pool[i].id < pool[j].id
		})
	}

	if len(pool) > width {
		pool = pool[:width]
	}

	w := hybridWindowOf(pool)
	w.Reserved = reserved
	return w
}

type hybridCandidate struct {
	id        string
	fts       int
	vec       int
	score     float64
	scope     map[string]string
	projectID string
	resolved  bool
}

func hybridWindowOf(cands []*hybridCandidate) HybridWindow {
	w := HybridWindow{IDs: make([]string, 0, len(cands)), Scores: make(map[string]float64, len(cands))}
	for _, c := range cands {
		w.IDs = append(w.IDs, c.id)
		w.Scores[c.id] = c.score
	}
	return w
}

// SearchHybrid combines FTS5 keyword search with vector similarity using
func (s *Store) SearchHybrid(ctx context.Context, projectID, query string, queryVec []float32, limit int) ([]Memory, error) {
	return s.SearchHybridScoped(ctx, projectID, query, queryVec, limit, nil)
}

// SearchHybridScoped is the production search entry point with a scope
func (s *Store) SearchHybridScoped(ctx context.Context, projectID, query string, queryVec []float32, limit int, scope map[string]string) ([]Memory, error) {
	p := DefaultSearchParams()
	p.MinSimilarity = s.vectorMinSimilarityFloor()
	p.Scope = scope
	return s.SearchHybridParams(ctx, projectID, query, queryVec, limit, p)
}

// SearchHybridParams is SearchHybrid with explicit fusion parameters. It is
func (s *Store) SearchHybridParams(ctx context.Context, projectID, query string, queryVec []float32, limit int, p SearchParams) ([]Memory, error) {
	final, _, err := s.searchHybridLegs(ctx, projectID, query, queryVec, limit, p)
	return final, err
}

type hybridLegs struct {
	fts []Memory
	vec []ScoredMemory
}

func (s *Store) searchHybridLegs(ctx context.Context, projectID, query string, queryVec []float32, limit int, p SearchParams) ([]Memory, hybridLegs, error) {
	p.ProjectID = projectID

	ftsResults, err := s.SearchFTS(ctx, projectID, query, limit*2)
	if err != nil {
		ftsResults = nil // non-fatal, proceed with vector only
	}
	legs := hybridLegs{fts: ftsResults}

	if queryVec == nil {
		final, err := s.fuseAndRank(ctx, ftsResults, nil, limit, keywordOnlyParams(p))
		return final, legs, err
	}

	vecResults, err := s.SearchVector(ctx, projectID, queryVec, limit*2)
	if err != nil {
		vecResults = nil // non-fatal, proceed with FTS only
	}
	legs.vec = vecResults
	filtered := filterVectorFloor(vecResults, p.MinSimilarity)
	stampVectorFloor(p.trace, vecResults, filtered, p.ProjectID)

	if len(filtered) == 0 {
		final, err := s.fuseAndRank(ctx, ftsResults, nil, limit, keywordOnlyParams(p))
		return final, legs, err
	}

	final, err := s.fuseAndRank(ctx, ftsResults, filtered, limit, p)
	return final, legs, err
}

func keywordOnlyParams(p SearchParams) SearchParams {
	p.FTSWeight = 1
	p.VecWeight = 0
	return p
}

const getByIDsChunk = 500

// GetByIDs fetches memories by a list of IDs.
func (s *Store) GetByIDs(ctx context.Context, ids []string) ([]Memory, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	columns := `
		SELECT ` + memoryColumns + `
		FROM memories
		WHERE id IN (%s)`

	var out []Memory
	for start := 0; start < len(ids); start += getByIDsChunk {
		end := min(start+getByIDsChunk, len(ids))
		chunk := ids[start:end]
		placeholders := make([]string, len(chunk))
		args := make([]interface{}, len(chunk))
		for i, id := range chunk {
			placeholders[i] = "?"
			args[i] = id
		}
		rows, err := s.queryDB().QueryContext(ctx, fmt.Sprintf(columns, strings.Join(placeholders, ",")), args...)
		if err != nil {
			return nil, fmt.Errorf("get by ids: %w", err)
		}
		found, err := scanMemories(rows)
		rows.Close() //nolint:errcheck
		if err != nil {
			return nil, fmt.Errorf("get by ids: %w", err)
		}
		out = append(out, found...)
	}
	return out, nil
}

func (s *Store) searchVectorAllLeg(ctx context.Context, queryVec []float32, limit int) ([]ScoredMemory, vectorLegFacts, error) {
	return s.searchVectorAllLegAt(ctx, queryVec, limit, time.Time{})
}

func (s *Store) searchVectorAllLegAt(ctx context.Context, queryVec []float32, limit int, now time.Time) ([]ScoredMemory, vectorLegFacts, error) {
	identity := s.configuredEmbeddingIdentity()

	cp, err := s.vectorCorpusFor(ctx, queryVec, identity)
	if err != nil {
		return nil, vectorLegFacts{}, err
	}
	if cp != nil {
		scratch := s.borrowVectorRows()
		defer s.returnVectorRows(scratch)
		v := s.viewVectorCorpus(cp, scratch, "", true, queryVec)
		defer releaseViewScratch(scratch, v)
		v.now = now
		return v.search(queryVec, limit, nil), v.facts, nil
	}

	rows := s.borrowVectorRows()
	defer s.returnVectorRows(rows)

	if err := s.snapshotVectors(ctx, vectorScanColumns, nil, "", nil, queryVec, identity, rows); err != nil {
		return nil, vectorLegFacts{}, err
	}
	rows.now = now
	return rows.search(queryVec, limit, nil), rows.facts, nil
}

// SearchVectorAll performs brute-force cosine similarity search across ALL projects.
func (s *Store) SearchVectorAll(ctx context.Context, queryVec []float32, limit int) ([]ScoredMemory, error) {
	rows, _, err := s.searchVectorAllLeg(ctx, queryVec, limit)
	return rows, err
}

// SearchHybridAll combines FTS5 and vector search across ALL projects using RRF.
func (s *Store) SearchHybridAll(ctx context.Context, query string, queryVec []float32, limit int) ([]Memory, error) {
	p := DefaultSearchParams()
	p.MinSimilarity = s.vectorMinSimilarityFloor()
	ftsResults, err := s.SearchFTSAll(ctx, query, limit*2)
	if err != nil {
		ftsResults = nil
	}

	if queryVec == nil {
		return s.fuseAndRank(ctx, ftsResults, nil, limit, keywordOnlyParams(p))
	}

	vecResults, err := s.SearchVectorAll(ctx, queryVec, limit*2)
	if err != nil {
		vecResults = nil
	}
	vecResults = filterVectorFloor(vecResults, p.MinSimilarity)

	if len(vecResults) == 0 {
		return s.fuseAndRank(ctx, ftsResults, nil, limit, keywordOnlyParams(p))
	}

	return s.fuseAndRank(ctx, ftsResults, vecResults, limit, p)
}

func float32sToBytes(fs []float32) []byte {
	buf := make([]byte, len(fs)*4)
	for i, f := range fs {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

func bytesToFloat32s(b []byte) []float32 {
	n := len(b) / 4
	fs := make([]float32, n)
	for i := range n {
		fs[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return fs
}

func cosineSimilarity(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}

	var dot, normA, normB float64
	for i := range a {
		ai, bi := float64(a[i]), float64(b[i])
		dot += ai * bi
		normA += ai * ai
		normB += bi * bi
	}

	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0
	}
	return float32(dot / denom)
}
