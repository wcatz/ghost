package memory

import (
	"context"
	"database/sql"
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
// not yet been processed by the linking worker. A vector from another vector
// space is not returned: it is a vector this process cannot compare anything
// with, and including it would both pair it across spaces and spend the
// memory's one scan slot, so the link would never be built after the row is
// re-embedded. The identity applied is the store's configured one
// (SetEmbeddingIdentity) — the linker has no embedding model of its own, so it
// asks the store what it can compare; with none configured, every row is
// returned as before.
func (s *Store) UnscannedEmbeddedMemoryIDs(ctx context.Context, projectID string, limit int) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `
		SELECT m.id
		FROM memories m
		JOIN memory_embeddings e ON e.memory_id = m.id
		LEFT JOIN link_scans ls ON ls.memory_id = m.id
		WHERE m.project_id = ? AND ls.memory_id IS NULL
		ORDER BY m.created_at DESC
		LIMIT ?
	`
	args := []any{projectID, limit}
	if s.embeddingIdentity != "" {
		query = `
		SELECT m.id
		FROM memories m
		JOIN memory_embeddings e ON e.memory_id = m.id
		LEFT JOIN link_scans ls ON ls.memory_id = m.id
		WHERE m.project_id = ? AND ls.memory_id IS NULL AND e.model = ?
		ORDER BY m.created_at DESC
		LIMIT ?
		`
		args = []any{projectID, s.embeddingIdentity, limit}
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
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
//
// A row recorded under a different vector identity than the configured one
// returns (nil, nil): this process has no usable vector for that memory, and
// handing the blob back would invite a cosine against vectors from another
// space — an arbitrary number that then becomes a `related` edge, a
// `supersedes` candidate, or both. A nil vector means "not available yet":
// the caller must leave the memory for a later pass, which is what it gets
// once the embedding worker has rewritten it in the configured space.
func (s *Store) GetEmbedding(ctx context.Context, memoryID string) ([]float32, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var blob []byte
	var model string
	err := s.db.QueryRowContext(ctx, `
		SELECT embedding, model FROM memory_embeddings WHERE memory_id = ?
	`, memoryID).Scan(&blob, &model)
	if err != nil {
		return nil, fmt.Errorf("get embedding: %w", err)
	}
	if identity := s.embeddingIdentity; identity != "" && model != identity {
		return nil, nil
	}
	return bytesToFloat32s(blob), nil
}

// EmbeddingStats returns how many memories have embeddings, how many of those
// embeddings are stale, and the total memory count, across all projects. Used
// by health/status diagnostics.
//
// "Has an embedding" means "has one this process can search with": with a
// vector identity configured, rows recorded under another one are counted as
// not embedded, because a re-embed is still pending for them and reporting
// them as covered is what would let `ghost mcp status` print a passing
// coverage line while the vector leg returns nothing.
//
// Those stale rows are returned separately rather than folded into a single
// gap, because the two halves of the gap are different diagnoses: stale rows
// are work the re-embed worker has queued and will finish, while a row with no
// vector at all has never been embedded — the state of a worker that never ran
// or of an install whose embedding was only just enabled. Both are reported by
// `ghost mcp status` and `ghost_health`.
//
// The three counts come from one statement so they describe one snapshot.
// Three separate COUNTs could straddle a write and report embedded+stale >
// total, which in a coverage line reads as corruption rather than as a race.
func (s *Store) EmbeddingStats(ctx context.Context) (embedded, stale, total int, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	identity := s.embeddingIdentity
	const countQuery = `
		SELECT
			(SELECT COUNT(*) FROM memory_embeddings WHERE ? = '' OR model = ?),
			(SELECT COUNT(*) FROM memory_embeddings WHERE ? <> '' AND model <> ?),
			(SELECT COUNT(*) FROM memories)
	`
	if err = s.db.QueryRowContext(ctx, countQuery, identity, identity, identity, identity).
		Scan(&embedded, &stale, &total); err != nil {
		return 0, 0, 0, fmt.Errorf("count embeddings: %w", err)
	}
	return embedded, stale, total, nil
}

// LinkStats returns the number of valid links and link-scanned memories
// across all projects. Used by health/status diagnostics.
func (s *Store) LinkStats(ctx context.Context) (links, scans int, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_links WHERE invalidated_at IS NULL`).Scan(&links); err != nil {
		return 0, 0, fmt.Errorf("count links: %w", err)
	}
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM link_scans`).Scan(&scans); err != nil {
		return 0, 0, fmt.Errorf("count link scans: %w", err)
	}
	return links, scans, nil
}

// SupersedesWithin returns the valid 'supersedes' edges whose BOTH endpoints
// are in ids: each pair is [superseder, superseded]. Used by ranking to demote
// a memory only when its actual replacement co-occurs in the same result set,
// so a superseded fact is never buried when its successor isn't even present.
//
// A pair whose endpoints' scopes conflict is withheld, on the same terms as
// SupersedePenalties: two claims about different places never stood in a
// supersession relation, and an edge that says they did is not a verdict any
// reader may act on. The edge stays in the graph.
//
// This function has no production caller today — SupersedePenalties is the
// reader ranking uses, and it applies the same exemption. The guard is here so
// that a caller added later inherits the store's rule rather than
// contradicting it, which is the trap best_practices.md warns about when an
// existing store API is exposed through a new path.
func (s *Store) SupersedesWithin(ctx context.Context, ids []string) ([][2]string, error) {
	if len(ids) < 2 {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	ph := make([]string, len(ids))
	args := make([]interface{}, 0, len(ids)*2)
	for i, id := range ids {
		ph[i] = "?"
		args = append(args, id)
	}
	for _, id := range ids {
		args = append(args, id)
	}
	list := strings.Join(ph, ",")
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT l.source_id, l.target_id, source_mem.scope, target_mem.scope
		FROM memory_links l
		JOIN memories source_mem ON source_mem.id = l.source_id
		JOIN memories target_mem ON target_mem.id = l.target_id
		WHERE l.relation = 'supersedes' AND l.invalidated_at IS NULL
		  AND l.source_id IN (%s) AND l.target_id IN (%s)
	`, list, list), args...)
	if err != nil {
		return nil, fmt.Errorf("supersedes within: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var pairs [][2]string
	for rows.Next() {
		var src, tgt string
		var sourceScope, targetScope sql.NullString
		if err := rows.Scan(&src, &tgt, &sourceScope, &targetScope); err != nil {
			return nil, err
		}
		if ScopesConflict(parseScope(sourceScope), parseScope(targetScope)) {
			continue
		}
		pairs = append(pairs, [2]string{src, tgt})
	}
	return pairs, rows.Err()
}

// LinksByRelationSource returns all valid (non-invalidated) links of the given
// relation and source whose SOURCE endpoint belongs to projectID. Used by
// ghost supersede to find previously-created 'supersedes'/llm links so it can
// reclassify them alongside freshly-discovered candidate pairs.
func (s *Store) LinksByRelationSource(ctx context.Context, projectID, relation, source string) ([]Link, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT l.source_id, l.target_id, l.relation, l.strength, l.source, l.created_at, l.invalidated_at
		FROM memory_links l
		JOIN memories m ON m.id = l.source_id
		WHERE m.project_id = ? AND l.relation = ? AND l.source = ? AND l.invalidated_at IS NULL
	`, projectID, relation, source)
	if err != nil {
		return nil, fmt.Errorf("links by relation source: %w", err)
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
