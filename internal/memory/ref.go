package memory

import (
	"context"
	"fmt"
)

// MemoryIDsByIDPrefix returns the ids of the memories in projectID whose id
// begins with prefix, in ascending id order.
//
// It is the resolution half of a targeted link withdrawal
// (`ghost supersede --withdraw`): an operator naming a memory types the id it
// was shown, and every Ghost report shortens it to eight characters, so a
// withdrawal that insisted on a full 32-character id could not be driven from
// the report that says which edge is wrong.
//
// The scope is projectID plus `_global`, and deliberately not wider. A promotion
// moves a memory into `_global` while keeping its links, so a live
// 'supersedes' edge can point from a project's own memory at a `_global` one
// and both endpoints have to be nameable from the project that owns the edge.
// Another project's memory is never returned: the caller uses this to name a
// row it is about to change, and a ref that resolved into a neighbouring
// project would make that possible.
//
// prefix is matched as LITERAL TEXT of its own length, not as a LIKE pattern, so
// neither SQL wildcard can widen the search, and case-insensitively because ids
// are hex and memIDKey already compares them that way. The match is therefore
// not served by the primary-key index; a withdrawal is a one-off operator action
// over a project's memories, not a retrieval path, so the scan is the right
// trade. A prefix that matches nothing returns an empty slice and no error —
// what a miss means is the caller's decision, and one of the callers here
// reports it as an ambiguity-free "not found".
func (s *Store) MemoryIDsByIDPrefix(ctx context.Context, projectID, prefix string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM memories
		WHERE (project_id = ? OR project_id = ?)
		  AND lower(substr(id, 1, ?)) = lower(?)
		ORDER BY id
	`, projectID, GlobalProjectID, len(prefix), prefix)
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
