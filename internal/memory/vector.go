package memory

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// StoreEmbedding saves an embedding vector for a memory.
// The vector is stored as raw little-endian float32 bytes.
func (s *Store) StoreEmbedding(ctx context.Context, memoryID string, vec []float32, model string) error {
	blob := float32sToBytes(vec)

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO memory_embeddings (memory_id, embedding, model)
		VALUES (?, ?, ?)
		ON CONFLICT(memory_id) DO UPDATE SET embedding = excluded.embedding, model = excluded.model, created_at = datetime('now')
	`, memoryID, blob, model)
	if err != nil {
		return fmt.Errorf("store embedding: %w", err)
	}
	return nil
}

// DeleteEmbedding removes the embedding for a memory.
func (s *Store) DeleteEmbedding(ctx context.Context, memoryID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM memory_embeddings WHERE memory_id = ?`, memoryID)
	return err
}

// UnembeddedMemoryIDs returns memory IDs that don't have embeddings yet.
func (s *Store) UnembeddedMemoryIDs(ctx context.Context, projectID string, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id
		FROM memories m
		LEFT JOIN memory_embeddings e ON e.memory_id = m.id
		WHERE m.project_id = ? AND e.memory_id IS NULL
		ORDER BY m.created_at DESC
		LIMIT ?
	`, projectID, limit)
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

// vecEntry holds a memory ID and its embedding for similarity search.
type vecEntry struct {
	memoryID  string
	embedding []float32
}

// SearchVector performs brute-force cosine similarity search against stored embeddings.
// Returns memory IDs sorted by descending similarity.
func (s *Store) SearchVector(ctx context.Context, projectID string, queryVec []float32, limit int) ([]ScoredMemory, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT e.memory_id, e.embedding, e.model
		FROM memory_embeddings e
		JOIN memories m ON m.id = e.memory_id
		WHERE m.project_id = ? OR m.project_id = '_global'
	`, projectID)
	if err != nil {
		return nil, fmt.Errorf("load embeddings: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var entries []vecEntry
	// Embeddings are stored per-model. A dimension mismatch means the rows
	// were written by a different model than the one producing queryVec, and
	// skipping them silently turns changed-model setups into "vector search
	// found nothing" with no explanation.
	mismatched, mismatchedModel := 0, ""
	for rows.Next() {
		var id, model string
		var blob []byte
		if err := rows.Scan(&id, &blob, &model); err != nil {
			return nil, err
		}
		vec := bytesToFloat32s(blob)
		if len(vec) == len(queryVec) {
			entries = append(entries, vecEntry{memoryID: id, embedding: vec})
			continue
		}
		mismatched++
		if mismatchedModel == "" {
			mismatchedModel = model
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if mismatched > 0 && s.logger != nil {
		s.logger.Warn("vector search skipped embeddings whose dimension does not match the query — the embedding model likely changed; re-embed to restore vector recall",
			"skipped", mismatched, "usable", len(entries), "query_dims", len(queryVec), "stored_model", mismatchedModel)
	}

	// Compute cosine similarity for each entry, dropping non-positive scores: a
	// cosine of 0 or below is not a match, and RRF awards weight by rank alone,
	// so a meaningless rank-1 candidate would otherwise claim the full vector
	// weight and enter the fused window.
	scored := make([]ScoredMemory, 0, len(entries))
	for _, e := range entries {
		if sim := cosineSimilarity(queryVec, e.embedding); sim > minVectorSimilarity {
			scored = append(scored, ScoredMemory{MemoryID: e.memoryID, Score: sim})
		}
	}

	sort.Slice(scored, func(i, j int) bool {
		return scored[i].Score > scored[j].Score
	})

	if len(scored) > limit {
		scored = scored[:limit]
	}
	return scored, nil
}

// minVectorSimilarity is the cosine floor below which a vector candidate is
// discarded before fusion. Only non-positive scores are dropped today — the
// conservative choice, since RRF already ranks by position and a higher floor
// risks evicting genuinely weak semantic matches.
const minVectorSimilarity float32 = 0

// ScoredMemory pairs a memory ID with a relevance score.
type ScoredMemory struct {
	MemoryID string
	Score    float32
}

// SearchParams parameterizes hybrid-search fusion. The zero value disables
// every signal — use DefaultSearchParams for production behavior. The bench
// harness (ghost bench --sweep) grid-searches these knobs against the graded
// dataset; defaults should only change on the strength of those numbers.
type SearchParams struct {
	FTSWeight float64 // RRF weight of the full-text leg
	VecWeight float64 // RRF weight of the vector leg
	RRFK      int     // RRF smoothing constant (Cormack & Clarke use 60)
	// DecayEnabled applies the category-aware time-decay factor
	// (decayFactor — the Go mirror of DecayRankingSQL) to reorder the result
	// window after truncation: decay reorders but never changes membership, so
	// a more-relevant memory is never dropped in favor of an unrelated younger
	// one. True by default (production behavior). The bench harness toggles it
	// to measure decay-on vs decay-off impact.
	DecayEnabled bool
	// SupersedeDemote, when true, demotes a memory below its superseder within
	// the result window when a valid 'supersedes' link between them exists AND
	// both are present. Unlike the recency prior it is targeted — it only ever
	// touches genuine replacement pairs, so it flips the staleness suite
	// without the collateral damage the recency-trap frontier showed. On by
	// default in production (DefaultSearchParams). See docs/benchmarks.md
	// Phase 3.
	SupersedeDemote bool
	// DecayReselect, when true, changes decayRank membership: keep the top
	// limit*2 by base score, then select the top limit by base×decay — so a
	// fresh memory ranked just below the pure-base cut can be rescued.
	// false (default) is the historical behavior: relevance owns membership
	// and decay only reorders the surviving window. Ship-gated on the
	// staleness and recency-trap suites — see docs/benchmarks.md Phase 3.
	DecayReselect bool
	// MinSimilarity is the cosine floor applied to vector-leg candidates
	// AFTER SearchVector's non-positive drop and BEFORE fusion. RRF awards by
	// rank alone, so without a floor every weak positive cosine still claims
	// a vector rank and can enter the fused window — padding results with
	// near-misses when the corpus has no true match. FTS candidates are
	// exempt (they have no cosine). 0 preserves the historical behavior of
	// only dropping non-positives. Production overrides this from config via
	// Store.SetVectorMinSimilarity (search.min_similarity).
	MinSimilarity float32
}

// DefaultSearchParams returns the production fusion parameters.
//
// Time-awareness comes from the always-on category-aware decay (DecayEnabled,
// see decayFactor) — not a blanket age-only freshness prior. The recency-trap
// suite in docs/benchmarks.md showed an untargeted prior damages correct-wins,
// which is why a flat RecencyWeight/RecencyTau prior was removed in favor of
// the category-aware factor (preference/convention/fact never decay, pattern/
// architecture tau 45, decision/gotcha/dependency tau 30).
//
// A link-graph expansion bonus was evaluated and removed. It was structurally
// dominated by simply retrieving a deeper vector-k: links are built from cosine
// similarity and the vector leg is also cosine, so the bonus only re-surfaced
// cosine-neighbors a larger k already reaches — a public LongMemEval-S kill
// experiment confirmed its recoveries were a strict subset of deeper-k's, with
// no headroom at production depth. The link graph is retained for Obsidian
// export and supersedes ranking. See docs/architecture.md.
//
// SupersedeDemote graduates to on here — the last step of the supersedes
// feature, which docs/benchmarks.md Phase 3 left deliberately separate because
// it changes live ranking. It is safe as a default for three reasons the
// benchmarks establish: it is a hard no-op unless a 'supersedes' edge joins
// two memories inside one result window; those edges only exist if the user
// ran `ghost supersede --apply`, which is itself opt-in and LLM-confirmed; and
// on the graded suites it flips staleness fresh-wins 0.083 -> 1.0 while leaving
// recency-trap correct-wins untouched at 0.929
// (TestSupersedeDemoteClearsFrontier). Leaving it off meant a memory the user
// had explicitly marked as replaced could still outrank its replacement in
// ghost_memory_search.
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

// filterVectorFloor drops vector candidates at or below floor (cosine).
// Membership-preserving for the FTS leg by construction — only ScoredMemory
// values are filtered. floor <= 0 is a no-op beyond SearchVector's own
// non-positive drop.
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

// demoteSuperseded reorders results so a superseded memory falls below every
// present memory that supersedes it. It penalizes each result by the number of
// its present superseders and stable-sorts by that penalty ascending — so a
// memory with no present superseder keeps its place, and an update chain
// (v3 supersedes v2 and v1; v2 supersedes v1 — star links) orders v3, v2, v1.
// A no-op when SupersedeDemote is off or no supersedes edge joins two results,
// so it never fires on unrelated memories (the recency-trap case). Errors are
// non-fatal: the unreordered results are returned.
func (s *Store) demoteSuperseded(ctx context.Context, results []Memory, p SearchParams) []Memory {
	if !p.SupersedeDemote || len(results) < 2 {
		return results
	}
	ids := make([]string, len(results))
	for i, m := range results {
		ids[i] = m.ID
	}
	s.mu.RLock()
	penalty, err := SupersedePenalties(ctx, s.db, ids)
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

// parseCreatedAt parses the SQLite datetime('now') format stored in
// memories.created_at. On failure it returns the zero time (ancient), so the
// caller applies no freshness boost.
func parseCreatedAt(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04:05", s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// decayFactor returns the category-aware time-decay multiplier for a memory
// given its age in days. It mirrors DecayRankingSQL (store.go) exactly — a
// pinned memory or a preference/convention/fact never decays (factor 1.0);
// pattern/architecture decay with tau 45 and a 0.3 floor; all other categories
// (decision, gotcha, dependency, ...) decay with tau 30 and a 0.15 floor. The
// SQL-vs-Go parity test (store_test.go) guards against drift between this and
// the SQL constant.
func DecayFactor(category string, pinned bool, ageDays float64) float64 {
	if pinned {
		return 1.0
	}
	switch category {
	case "preference", "convention", "fact":
		return 1.0
	case "pattern", "architecture":
		return math.Max(0.3, 1.0/(1.0+ageDays/45.0))
	default:
		return math.Max(0.15, 1.0/(1.0+ageDays/30.0))
	}
}

// decayRank truncates results to limit, then (when enabled) reorders by base
// score × decayFactor. base is the fused score when scores is non-nil;
// otherwise it is synthesized from position (the FTS-only paths),
// base = 1/(RRFK+rank+1).
//
// Membership rules:
//   - DecayReselect=false (default, historical): truncate to limit by base
//     alone — relevance owns membership; decay only reorders the surviving
//     window. A fresh memory ranked below the cut by base is NOT rescued.
//   - DecayReselect=true: keep the top limit*2 by base, then select the top
//     limit by base×decay — a fresh memory just below the pure-base cut can
//     enter the final window. Ship-gated on staleness + recency-trap suites.
//
// Age reads created_at — never updated_at, which Upsert's strengthen path
// bumps. An unparseable created_at is treated as ancient so a malformed
// timestamp can never spuriously win. The sort by base happens in both modes
// — the fused path hydrates via GetByIDs, which does NOT preserve order.
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
	}

	// Sort by base first: relevance is the primary key in both modes.
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].base != scored[j].base {
			return scored[i].base > scored[j].base
		}
		return scored[i].m.ID < scored[j].m.ID
	})

	baseCut := limit
	if p.DecayReselect && p.DecayEnabled {
		// Keep a wider base window so decay can choose among more candidates.
		baseCut = limit * 2
	}
	if len(scored) > baseCut {
		scored = scored[:baseCut]
	}

	if p.DecayEnabled {
		// Reorder by base × decay (ordering; with DecayReselect this also
		// owns final membership via the subsequent truncate).
		sort.SliceStable(scored, func(i, j int) bool {
			fi := scored[i].base * DecayFactor(scored[i].m.Category, scored[i].m.Pinned, ageDays(scored[i].m.CreatedAt, now))
			fj := scored[j].base * DecayFactor(scored[j].m.Category, scored[j].m.Pinned, ageDays(scored[j].m.CreatedAt, now))
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

// ageDays returns a memory's age in days from its created_at, clamped at 0
// (a future timestamp is treated as brand-new).
func ageDays(createdAt string, now time.Time) float64 {
	ageDays := now.Sub(parseCreatedAt(createdAt)).Hours() / 24.0
	if ageDays < 0 {
		return 0
	}
	return ageDays
}

// fuseAndRank runs the shared hybrid pipeline: RRF-fuse the two result legs,
// hydrate the candidate pool, then rank and truncate (inside decayRank).
func (s *Store) fuseAndRank(ctx context.Context, ftsResults []Memory, vecResults []ScoredMemory, limit int, p SearchParams) ([]Memory, error) {
	window := FuseAndSelectWindow(ftsResults, vecResults, limit, p)

	// Hydrate the full candidate pool before ranking — decayRank needs
	// category/pinned/created_at to reorder the window, and hydration via
	// GetByIDs does not preserve order, so the final sort lives there too.
	memories, err := s.GetByIDs(ctx, window.IDs)
	if err != nil {
		return nil, err
	}

	memories = decayRank(memories, window.Scores, p, limit, time.Now().UTC())
	return s.demoteResults(ctx, memories, p), nil
}

// HybridWindow is the result of hybrid fusion and window selection: the memory
// ids eligible to be returned, in ranked order, with the fused score each was
// selected on.
type HybridWindow struct {
	IDs    []string
	Scores map[string]float64
}

// FuseAndSelectWindow owns both halves of hybrid retrieval — fusing the
// keyword and vector legs into one ranking, and deciding which memories form
// the result window — in one place, because window selection is not separable
// from fusion.
//
// Selecting by fused score alone is what made the window unable to admit a
// keyword-only hit. RRF weights the vector leg 0.7 and the keyword leg 0.3, so
// with k=60 the keyword leg's rank-1 row scores 0.3/61 ≈ 0.0049, while the
// vector leg's 20th row — still comfortably inside the fetched window — scores
// 0.7/80 ≈ 0.0088. A full vector leg therefore outranked the best keyword
// match, every time, and no amount of exact identifier matching could put that
// memory in the results. That is the case FTS exists for.
//
// So the window reserves slots for the top keyword hits. Reserving is the
// narrowest rule that repairs it: a reserved hit already in the window keeps
// its place, and one the score cut would have dropped is promoted in its
// stead. It does not invert the ranking — a memory matching both legs
// accumulates both weighted contributions and still outranks a keyword-only
// row, because the reservation only guarantees admission, never a position.
//
// The returned width is `limit`, except under DecayReselect, where decay
// narrows the set afterwards and therefore still needs the wider pool it has
// always been given. Either way this is the eligible set, and decayRank orders
// and trims within it.
func FuseAndSelectWindow(ftsResults []Memory, vecResults []ScoredMemory, limit int, p SearchParams) HybridWindow {
	// Fusion: reciprocal rank fusion over both legs. A memory found by both
	// accumulates both contributions, which is the only thing that makes a
	// high FTS rank outrank a high vector rank.
	byID := make(map[string]*hybridCandidate, len(ftsResults)+len(vecResults))
	get := func(id string) *hybridCandidate {
		c, ok := byID[id]
		if !ok {
			c = &hybridCandidate{id: id}
			byID[id] = c
		}
		return c
	}
	for rank, m := range ftsResults {
		c := get(m.ID)
		c.fts = rank + 1
		c.score += p.FTSWeight / float64(p.RRFK+rank+1)
	}
	for rank, sm := range vecResults {
		c := get(sm.MemoryID)
		c.vec = rank + 1
		c.score += p.VecWeight / float64(p.RRFK+rank+1)
	}

	pool := make([]*hybridCandidate, 0, len(byID))
	for _, c := range byID {
		pool = append(pool, c)
	}
	// Deterministic order: map iteration is randomized, and an unstable sort
	// over tied scores made result order — and the demotion penalties that
	// depend on it — vary between runs of the same query.
	sort.Slice(pool, func(i, j int) bool {
		if pool[i].score != pool[j].score {
			return pool[i].score > pool[j].score
		}
		return pool[i].id < pool[j].id
	})

	width := limit
	if p.DecayReselect && p.DecayEnabled {
		width = limit * 2
	}
	if width <= 0 {
		return HybridWindow{}
	}

	// Reserve slots for the top keyword hits: the best `limit/5` of them are
	// guaranteed a place in the window even when the vector leg would have
	// filled every row. A fifth of the window, and only when that is at least
	// one — below a window of five, a reservation would be a majority of the
	// answer rather than a correction to it.
	//
	// Admission is the whole of it, and the position stays the fused score's
	// to decide. Two stronger interventions were built and measured against
	// the built-in dataset before settling here:
	//
	//   - Reordering the returned slice so reserved hits lead the window.
	//     decayRank re-sorts by fused score on the way out, so this changes
	//     nothing beyond admission: the hit reappeared at the bottom.
	//   - Flooring a reserved hit's score. The top is far too strong — hybrid
	//     R@1 0.507 -> 0.366, NDCG@10 0.812 -> 0.738. The median improves
	//     R@10 and NDCG but is a score, and decay multiplies scores by a
	//     category- and age-dependent factor, so a reserved hit parked a hair
	//     above the median gets reordered by decay and the invariant that
	//     uniform timestamps leave the graded ranking untouched
	//     (TestDecayDoesNotPerturbGradedBench) stops holding. Every margin big
	//     enough to survive that spread exceeds the catastrophic top floor.
	//
	// What the issue describes is admission, and admission is what this does.
	if slots := limit / 5; slots > 0 && len(pool) > width {
		isReserved := func(c *hybridCandidate) bool {
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
			// Evict the weakest admitted row that is not itself reserved.
			for i := width - 1; i >= 0; i-- {
				if !isReserved(pool[i]) {
					pool[i] = c
					break
				}
			}
			admitted[c.id] = true
		}
		// The evicted row may have been stronger than its replacement.
		sort.Slice(pool[:width], func(i, j int) bool {
			if pool[i].score != pool[j].score {
				return pool[i].score > pool[j].score
			}
			return pool[i].id < pool[j].id
		})
	}

	// The cut, once the reservation has had its say. It belongs out here
	// rather than inside the reservation: a limit too small to reserve
	// anything (below five) still has to return a window, not the pool.
	if len(pool) > width {
		pool = pool[:width]
	}

	return hybridWindowOf(pool)
}

//

// hybridCandidate is one memory's fused standing: the rank each leg gave it
// and the score those ranks produced. fts or vec is 0 when that leg did not
// retrieve it, which is what distinguishes a two-leg hit from a keyword-only
// one.
type hybridCandidate struct {
	id    string
	fts   int
	vec   int
	score float64
}

// hybridWindowOf materialises the selected candidates, keeping the fused score
// alongside each id for decayRank.
func hybridWindowOf(cands []*hybridCandidate) HybridWindow {
	w := HybridWindow{IDs: make([]string, 0, len(cands)), Scores: make(map[string]float64, len(cands))}
	for _, c := range cands {
		w.IDs = append(w.IDs, c.id)
		w.Scores[c.id] = c.score
	}
	return w
}

// SearchHybrid combines FTS5 keyword search with vector similarity using
// Reciprocal Rank Fusion (RRF). Falls back to FTS-only if queryVec is nil.
func (s *Store) SearchHybrid(ctx context.Context, projectID, query string, queryVec []float32, limit int) ([]Memory, error) {
	p := DefaultSearchParams()
	p.MinSimilarity = s.vectorMinSimilarityFloor()
	return s.SearchHybridParams(ctx, projectID, query, queryVec, limit, p)
}

// SearchHybridParams is SearchHybrid with explicit fusion parameters. It
// exists for the benchmark harness; production callers use SearchHybrid.
func (s *Store) SearchHybridParams(ctx context.Context, projectID, query string, queryVec []float32, limit int, p SearchParams) ([]Memory, error) {
	// FTS results.
	ftsResults, err := s.SearchFTS(ctx, projectID, query, limit*2)
	if err != nil {
		ftsResults = nil // non-fatal, proceed with vector only
	}

	// If no vector, return FTS results directly.
	if queryVec == nil {
		return s.demoteResults(ctx, decayRank(ftsResults, nil, p, limit, time.Now().UTC()), p), nil
	}

	// Vector results.
	vecResults, err := s.SearchVector(ctx, projectID, queryVec, limit*2)
	if err != nil {
		vecResults = nil // non-fatal, proceed with FTS only
	}
	vecResults = filterVectorFloor(vecResults, p.MinSimilarity)

	// If only FTS worked, return that.
	if len(vecResults) == 0 {
		return s.demoteResults(ctx, decayRank(ftsResults, nil, p, limit, time.Now().UTC()), p), nil
	}

	return s.fuseAndRank(ctx, ftsResults, vecResults, limit, p)
}

// GetByIDs fetches memories by a list of IDs.
func (s *Store) GetByIDs(ctx context.Context, ids []string) ([]Memory, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	placeholders := make([]string, len(ids))
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}

	query := fmt.Sprintf(`
		SELECT id, project_id, category, content, importance, access_count,
		       last_accessed, source, tags, pinned, resolved_at, created_at, updated_at,
		       agent, session_id, source_ref, confidence, scope
		FROM memories
		WHERE id IN (%s)
	`, strings.Join(placeholders, ","))

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("get by ids: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	return scanMemories(rows)
}

// SearchVectorAll performs brute-force cosine similarity search across ALL projects.
func (s *Store) SearchVectorAll(ctx context.Context, queryVec []float32, limit int) ([]ScoredMemory, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT memory_id, embedding, model FROM memory_embeddings
	`)
	if err != nil {
		return nil, fmt.Errorf("load embeddings: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var entries []vecEntry
	// Embeddings are stored per-model. A dimension mismatch means the rows
	// were written by a different model than the one producing queryVec, and
	// skipping them silently turns changed-model setups into "vector search
	// found nothing" with no explanation.
	mismatched, mismatchedModel := 0, ""
	for rows.Next() {
		var id, model string
		var blob []byte
		if err := rows.Scan(&id, &blob, &model); err != nil {
			return nil, err
		}
		vec := bytesToFloat32s(blob)
		if len(vec) == len(queryVec) {
			entries = append(entries, vecEntry{memoryID: id, embedding: vec})
			continue
		}
		mismatched++
		if mismatchedModel == "" {
			mismatchedModel = model
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if mismatched > 0 && s.logger != nil {
		s.logger.Warn("vector search skipped embeddings whose dimension does not match the query — the embedding model likely changed; re-embed to restore vector recall",
			"skipped", mismatched, "usable", len(entries), "query_dims", len(queryVec), "stored_model", mismatchedModel)
	}

	scored := make([]ScoredMemory, 0, len(entries))
	for _, e := range entries {
		if sim := cosineSimilarity(queryVec, e.embedding); sim > minVectorSimilarity {
			scored = append(scored, ScoredMemory{MemoryID: e.memoryID, Score: sim})
		}
	}
	sort.Slice(scored, func(i, j int) bool { return scored[i].Score > scored[j].Score })
	if len(scored) > limit {
		scored = scored[:limit]
	}
	return scored, nil
}

// SearchHybridAll combines FTS5 and vector search across ALL projects using RRF.
// Falls back to FTS-only when queryVec is nil.
func (s *Store) SearchHybridAll(ctx context.Context, query string, queryVec []float32, limit int) ([]Memory, error) {
	p := DefaultSearchParams()
	p.MinSimilarity = s.vectorMinSimilarityFloor()
	ftsResults, err := s.SearchFTSAll(ctx, query, limit*2)
	if err != nil {
		ftsResults = nil
	}

	// The FTS-only fallbacks below must apply the same supersede demote the
	// fused path does — otherwise cross-project search silently ranks
	// superseded memories above their replacements whenever Ollama is down.
	if queryVec == nil {
		return s.demoteResults(ctx, decayRank(ftsResults, nil, p, limit, time.Now().UTC()), p), nil
	}

	vecResults, err := s.SearchVectorAll(ctx, queryVec, limit*2)
	if err != nil {
		vecResults = nil
	}
	vecResults = filterVectorFloor(vecResults, p.MinSimilarity)

	if len(vecResults) == 0 {
		return s.demoteResults(ctx, decayRank(ftsResults, nil, p, limit, time.Now().UTC()), p), nil
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
