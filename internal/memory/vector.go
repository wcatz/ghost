package memory

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"strings"
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
	defer rows.Close()

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
		SELECT e.memory_id, e.embedding
		FROM memory_embeddings e
		JOIN memories m ON m.id = e.memory_id
		WHERE m.project_id = ? OR m.project_id = '_global'
	`, projectID)
	if err != nil {
		return nil, fmt.Errorf("load embeddings: %w", err)
	}
	defer rows.Close()

	var entries []vecEntry
	for rows.Next() {
		var id string
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			return nil, err
		}
		vec := bytesToFloat32s(blob)
		if len(vec) == len(queryVec) {
			entries = append(entries, vecEntry{memoryID: id, embedding: vec})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Compute cosine similarity for each entry.
	scored := make([]ScoredMemory, len(entries))
	for i, e := range entries {
		scored[i] = ScoredMemory{
			MemoryID: e.memoryID,
			Score:    cosineSimilarity(queryVec, e.embedding),
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
	FTSWeight   float64 // RRF weight of the full-text leg
	VecWeight   float64 // RRF weight of the vector leg
	RRFK        int     // RRF smoothing constant (Cormack & Clarke use 60)
	GraphWeight float64 // additive link-graph bonus weight; 0 skips the graph pass
	GraphSeeds  int     // top-ranked results used as graph-expansion seeds
	GraphHops   int     // link-traversal depth from each seed
}

// DefaultSearchParams returns the production fusion parameters.
func DefaultSearchParams() SearchParams {
	return SearchParams{
		FTSWeight:   0.3,
		VecWeight:   0.7,
		RRFK:        60,
		GraphWeight: 0.15,
		GraphSeeds:  3,
		GraphHops:   2,
	}
}

// applyGraphBonus adds an additive RRF-style bonus to scores for memories
// reachable via links from the top-ranked seed IDs. Additive-only: when no
// links exist, scores are unchanged. Errors are non-fatal (the graph signal
// is best-effort, like the FTS and vector legs). Pass projectID "" to span
// all projects.
func (s *Store) applyGraphBonus(ctx context.Context, projectID string, scores map[string]float64, idSet map[string]bool, ranked []string, limit int, p SearchParams) {
	seeds := ranked
	if len(seeds) > p.GraphSeeds {
		seeds = seeds[:p.GraphSeeds]
	}

	// Walk from each seed separately so a seed is only excluded from its own
	// expansion — a lower-ranked candidate linked to the top hit still gets
	// the bonus. The bonus decays with seed rank.
	for seedRank, seed := range seeds {
		neighbors, err := s.GraphNeighbors(ctx, projectID, []string{seed}, p.GraphHops, limit)
		if err != nil {
			s.logger.Debug("graph bonus: traversal failed", "error", err, "seed", seed)
			return
		}
		for _, n := range neighbors {
			scores[n.MemoryID] += p.GraphWeight * float64(n.Strength) / float64(p.RRFK+seedRank+1)
			idSet[n.MemoryID] = true
		}
	}
}

// fuseAndRank runs the shared hybrid pipeline: RRF-fuse the two result legs,
// optionally apply the link-graph bonus, then rank, truncate, and hydrate.
// projectID scopes the graph traversal; "" spans all projects.
func (s *Store) fuseAndRank(ctx context.Context, projectID string, ftsResults []Memory, vecResults []ScoredMemory, limit int, p SearchParams) ([]Memory, error) {
	scores := make(map[string]float64)
	idSet := make(map[string]bool)
	for rank, m := range ftsResults {
		scores[m.ID] += p.FTSWeight / float64(p.RRFK+rank+1)
		idSet[m.ID] = true
	}
	for rank, sm := range vecResults {
		scores[sm.MemoryID] += p.VecWeight / float64(p.RRFK+rank+1)
		idSet[sm.MemoryID] = true
	}

	if p.GraphWeight > 0 && p.GraphSeeds > 0 {
		// Preliminary ranking to pick graph seeds.
		prelim := make([]string, 0, len(idSet))
		for id := range idSet {
			prelim = append(prelim, id)
		}
		sort.Slice(prelim, func(i, j int) bool { return scores[prelim[i]] > scores[prelim[j]] })

		// Third signal: link-graph expansion from top seeds (additive-only).
		s.applyGraphBonus(ctx, projectID, scores, idSet, prelim, limit, p)
	}

	// Sort by fused score, truncate, and hydrate.
	ranked := make([]string, 0, len(idSet))
	for id := range idSet {
		ranked = append(ranked, id)
	}
	sort.Slice(ranked, func(i, j int) bool { return scores[ranked[i]] > scores[ranked[j]] })
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}

	memories, err := s.GetByIDs(ctx, ranked)
	if err != nil {
		return nil, err
	}
	// Re-sort by fused score (GetByIDs doesn't preserve order).
	sort.Slice(memories, func(i, j int) bool {
		return scores[memories[i].ID] > scores[memories[j].ID]
	})
	return memories, nil
}

// SearchHybrid combines FTS5 keyword search with vector similarity using
// Reciprocal Rank Fusion (RRF), plus an additive link-graph expansion bonus.
// Falls back to FTS-only if queryVec is nil.
func (s *Store) SearchHybrid(ctx context.Context, projectID, query string, queryVec []float32, limit int) ([]Memory, error) {
	return s.SearchHybridParams(ctx, projectID, query, queryVec, limit, DefaultSearchParams())
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
		if len(ftsResults) > limit {
			ftsResults = ftsResults[:limit]
		}
		return ftsResults, nil
	}

	// Vector results.
	vecResults, err := s.SearchVector(ctx, projectID, queryVec, limit*2)
	if err != nil {
		vecResults = nil // non-fatal, proceed with FTS only
	}

	// If only FTS worked, return that.
	if len(vecResults) == 0 {
		if len(ftsResults) > limit {
			ftsResults = ftsResults[:limit]
		}
		return ftsResults, nil
	}

	return s.fuseAndRank(ctx, projectID, ftsResults, vecResults, limit, p)
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
		       last_accessed, source, tags, pinned, created_at, updated_at
		FROM memories
		WHERE id IN (%s)
	`, strings.Join(placeholders, ","))

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("get by ids: %w", err)
	}
	defer rows.Close()
	return scanMemories(rows)
}

// SearchVectorAll performs brute-force cosine similarity search across ALL projects.
func (s *Store) SearchVectorAll(ctx context.Context, queryVec []float32, limit int) ([]ScoredMemory, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT memory_id, embedding FROM memory_embeddings
	`)
	if err != nil {
		return nil, fmt.Errorf("load embeddings: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var entries []vecEntry
	for rows.Next() {
		var id string
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			return nil, err
		}
		vec := bytesToFloat32s(blob)
		if len(vec) == len(queryVec) {
			entries = append(entries, vecEntry{memoryID: id, embedding: vec})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	scored := make([]ScoredMemory, len(entries))
	for i, e := range entries {
		scored[i] = ScoredMemory{MemoryID: e.memoryID, Score: cosineSimilarity(queryVec, e.embedding)}
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
	ftsResults, err := s.SearchFTSAll(ctx, query, limit*2)
	if err != nil {
		ftsResults = nil
	}

	if queryVec == nil {
		if len(ftsResults) > limit {
			ftsResults = ftsResults[:limit]
		}
		return ftsResults, nil
	}

	vecResults, err := s.SearchVectorAll(ctx, queryVec, limit*2)
	if err != nil {
		vecResults = nil
	}

	if len(vecResults) == 0 {
		if len(ftsResults) > limit {
			ftsResults = ftsResults[:limit]
		}
		return ftsResults, nil
	}

	return s.fuseAndRank(ctx, "", ftsResults, vecResults, limit, DefaultSearchParams())
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
