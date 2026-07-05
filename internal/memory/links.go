package memory

import (
	"context"
	"fmt"
	"strings"
)

// Link is an edge between two memories. 'related' links are symmetric and
// stored with (source_id < target_id) normalized ordering; directed relations
// (supersedes, contradicts, elaborates, causes) preserve their direction.
type Link struct {
	SourceID      string
	TargetID      string
	Relation      string
	Strength      float32
	Source        string
	CreatedAt     string
	InvalidatedAt *string
}

// symmetricRelations are stored in normalized (min, max) ID order so A→B and
// B→A collapse to one row.
var symmetricRelations = map[string]bool{"related": true}

// CreateLink inserts an edge between two memories. Idempotent: re-inserting
// an existing (source, target, relation) keeps the higher strength and
// clears any invalidation.
func (s *Store) CreateLink(ctx context.Context, sourceID, targetID, relation string, strength float32, source string) error {
	if sourceID == targetID {
		return fmt.Errorf("create link: self-links not allowed (id %s)", sourceID)
	}
	if symmetricRelations[relation] && sourceID > targetID {
		sourceID, targetID = targetID, sourceID
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO memory_links (source_id, target_id, relation, strength, source)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(source_id, target_id, relation) DO UPDATE SET
			strength = MAX(strength, excluded.strength),
			invalidated_at = NULL
	`, sourceID, targetID, relation, strength, source)
	if err != nil {
		return fmt.Errorf("create link: %w", err)
	}
	return nil
}

// GetLinks returns all valid (non-invalidated) links touching a memory,
// from either endpoint, strongest first.
func (s *Store) GetLinks(ctx context.Context, memoryID string) ([]Link, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT source_id, target_id, relation, strength, source, created_at, invalidated_at
		FROM memory_links
		WHERE (source_id = ? OR target_id = ?) AND invalidated_at IS NULL
		ORDER BY strength DESC
	`, memoryID, memoryID)
	if err != nil {
		return nil, fmt.Errorf("get links: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var links []Link
	for rows.Next() {
		var l Link
		if err := rows.Scan(&l.SourceID, &l.TargetID, &l.Relation, &l.Strength, &l.Source, &l.CreatedAt, &l.InvalidatedAt); err != nil {
			return nil, err
		}
		links = append(links, l)
	}
	return links, rows.Err()
}

// MarkLinkScanned records that the linking worker has processed this memory.
// Rows cascade-delete with the memory, so reflection churn resets scans
// automatically and the worker self-heals.
func (s *Store) MarkLinkScanned(ctx context.Context, memoryID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO link_scans (memory_id) VALUES (?)
		ON CONFLICT(memory_id) DO UPDATE SET scanned_at = datetime('now')
	`, memoryID)
	if err != nil {
		return fmt.Errorf("mark link scanned: %w", err)
	}
	return nil
}

// UnscannedEmbeddedMemoryIDs returns memories that have an embedding but have
// not yet been processed by the linking worker.
func (s *Store) UnscannedEmbeddedMemoryIDs(ctx context.Context, projectID string, limit int) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id
		FROM memories m
		JOIN memory_embeddings e ON e.memory_id = m.id
		LEFT JOIN link_scans ls ON ls.memory_id = m.id
		WHERE m.project_id = ? AND ls.memory_id IS NULL
		ORDER BY m.created_at DESC
		LIMIT ?
	`, projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("unscanned embedded memories: %w", err)
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

// GetEmbedding returns the stored embedding vector for a memory.
func (s *Store) GetEmbedding(ctx context.Context, memoryID string) ([]float32, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var blob []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT embedding FROM memory_embeddings WHERE memory_id = ?
	`, memoryID).Scan(&blob)
	if err != nil {
		return nil, fmt.Errorf("get embedding: %w", err)
	}
	return bytesToFloat32s(blob), nil
}

// GraphNeighbor is a memory reached by traversing links from seed memories.
type GraphNeighbor struct {
	MemoryID string
	Depth    int
	Strength float32 // product of link strengths along the strongest path
}

// GraphNeighbors walks the link graph outward from seed memory IDs up to
// maxHops, returning reachable memories (excluding the seeds themselves)
// ordered by path strength. Traversal is bidirectional and only follows
// valid (non-invalidated) links. Results are scoped to projectID plus
// '_global'; pass projectID "" to search all projects.
func (s *Store) GraphNeighbors(ctx context.Context, projectID string, seedIDs []string, maxHops, limit int) ([]GraphNeighbor, error) {
	if len(seedIDs) == 0 {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	placeholders := make([]string, len(seedIDs))
	args := make([]interface{}, 0, len(seedIDs)+5)
	for i, id := range seedIDs {
		placeholders[i] = "(?)"
		args = append(args, id)
	}
	args = append(args, maxHops, projectID, projectID, limit)

	// The project predicate is applied at every hop (not just on final
	// rows) so traversal never routes through another project's memories.
	query := fmt.Sprintf(`
		WITH RECURSIVE seeds(id) AS (VALUES %s),
		walk(id, depth, strength) AS (
			SELECT id, 0, 1.0 FROM seeds
			UNION
			SELECT n.id, w.depth + 1, w.strength * l.strength
			FROM memory_links l
			JOIN walk w ON w.id IN (l.source_id, l.target_id)
			JOIN memories n ON n.id = CASE WHEN l.source_id = w.id THEN l.target_id ELSE l.source_id END
			WHERE w.depth < ? AND l.invalidated_at IS NULL
			  AND (? = '' OR n.project_id = ? OR n.project_id = '_global')
		)
		SELECT w.id, MIN(w.depth) AS depth, MAX(w.strength) AS strength
		FROM walk w
		WHERE w.depth > 0
		  AND w.id NOT IN (SELECT id FROM seeds)
		GROUP BY w.id
		ORDER BY strength DESC
		LIMIT ?
	`, strings.Join(placeholders, ","))

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("graph neighbors: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var neighbors []GraphNeighbor
	for rows.Next() {
		var n GraphNeighbor
		if err := rows.Scan(&n.MemoryID, &n.Depth, &n.Strength); err != nil {
			return nil, err
		}
		neighbors = append(neighbors, n)
	}
	return neighbors, rows.Err()
}

// InvalidateLink soft-invalidates a link (Zep-style: never delete, mark
// invalid with a timestamp so history is preserved).
func (s *Store) InvalidateLink(ctx context.Context, sourceID, targetID, relation string) error {
	if symmetricRelations[relation] && sourceID > targetID {
		sourceID, targetID = targetID, sourceID
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx, `
		UPDATE memory_links SET invalidated_at = datetime('now')
		WHERE source_id = ? AND target_id = ? AND relation = ?
	`, sourceID, targetID, relation)
	if err != nil {
		return fmt.Errorf("invalidate link: %w", err)
	}
	return nil
}
