package memory

import (
	"context"
	"fmt"
	"unicode/utf8"
)

func (s *Store) MemoryIDsByIDPrefix(ctx context.Context, projectID, prefix string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM memories
		WHERE (project_id = ? OR project_id = ?)
		  AND lower(substr(id, 1, ?)) = lower(?)
		ORDER BY id
	`, projectID, GlobalProjectID, utf8.RuneCountInString(prefix), prefix)
	if err != nil {
		return nil, fmt.Errorf("memory ids by prefix: %w", err)
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

func (s *Store) MemoryIDsByIDPrefixAnyProject(ctx context.Context, prefix string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM memories
		WHERE lower(substr(id, 1, ?)) = lower(?)
		ORDER BY id
	`, utf8.RuneCountInString(prefix), prefix)
	if err != nil {
		return nil, fmt.Errorf("memory ids by prefix (any project): %w", err)
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

func (s *Store) AnyMemoryIDsByIDPrefix(ctx context.Context, prefix string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM (
		    SELECT id FROM memories
		    WHERE lower(substr(id, 1, ?)) = lower(?)
		    UNION
		    SELECT memory_id AS id FROM memory_history
		    WHERE lower(substr(memory_id, 1, ?)) = lower(?)
		) ORDER BY id
	`, utf8.RuneCountInString(prefix), prefix, utf8.RuneCountInString(prefix), prefix)
	if err != nil {
		return nil, fmt.Errorf("memory ids by prefix (any memory): %w", err)
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
