package memory

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
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

// SearchHybrid combines FTS5 keyword search with vector similarity using
// Reciprocal Rank Fusion (RRF). Falls back to FTS-only if queryVec is nil.
func (s *Store) SearchHybrid(ctx context.Context, projectID, query string, queryVec []float32, limit int) ([]Memory, error) {
	const k = 60 // RRF smoothing constant

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

	// RRF fusion: score = 0.3/(k+fts_rank) + 0.7/(k+vec_rank)
	scores := make(map[string]float64)
	idSet := make(map[string]bool)

	for rank, m := range ftsResults {
		scores[m.ID] += 0.3 / float64(k+rank+1)
		idSet[m.ID] = true
	}
	for rank, sm := range vecResults {
		scores[sm.MemoryID] += 0.7 / float64(k+rank+1)
		idSet[sm.MemoryID] = true
	}

	// Sort by fused score.
	type idScore struct {
		id    string
		score float64
	}
	ranked := make([]idScore, 0, len(idSet))
	for id := range idSet {
		ranked = append(ranked, idScore{id: id, score: scores[id]})
	}
	sort.Slice(ranked, func(i, j int) bool {
		return ranked[i].score > ranked[j].score
	})

	if len(ranked) > limit {
		ranked = ranked[:limit]
	}

	// Build ID list for batch fetch.
	ids := make([]string, len(ranked))
	for i, r := range ranked {
		ids[i] = r.id
	}

	memories, err := s.GetByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}

	// Re-sort by fused score (GetByIDs doesn't preserve order).
	scoreMap := scores
	sort.Slice(memories, func(i, j int) bool {
		return scoreMap[memories[i].ID] > scoreMap[memories[j].ID]
	})

	return memories, nil
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
	`, joinStrings(placeholders, ","))

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("get by ids: %w", err)
	}
	defer rows.Close()
	return scanMemories(rows)
}

func joinStrings(ss []string, sep string) string {
	result := ""
	for i, s := range ss {
		if i > 0 {
			result += sep
		}
		result += s
	}
	return result
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
