package memory

import (
	"context"
	"fmt"
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
