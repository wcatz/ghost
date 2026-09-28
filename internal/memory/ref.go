package memory

import (
	"context"
	"fmt"
	"unicode/utf8"
)

// MemoryIDsByIDPrefix returns the ids of the memories in projectID whose id
// begins with prefix, in ascending id order.
//
// It is the resolution half of a targeted link withdrawal
// (`ghost supersede --withdraw`): an operator naming a memory types the id it
// was shown, and every Ghost report shortens it to eight characters, so a
// withdrawal that insisted on a full 32-character id could not be driven from
// the report that says which edge is wrong. Because the match is a PREFIX, a
// full id resolves through the same call — which matters because ids are not
// necessarily hex: `ghost import` writes an artifact's ids verbatim, and the
// column only defaults to hex(randomblob(16)). A caller that rejected a
// non-hex-shaped ref would make such a row unnameable, and an edge endpoint that
// cannot be named is a repair nobody can perform.
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
// are hex and memIDKey already compares them that way.
//
// The bound is a RUNE COUNT, not a byte length, and that is not pedantry: the id
// column is TEXT and SQLite's substr() slices by CHARACTER, so a ref holding a
// multi-byte rune ("日本" is 6 bytes, 2 characters) would be compared against the
// first six CHARACTERS of every id and could never match — a false negative that
// refuses an id the store really has, and refuses it while claiming no memory has
// it. The two lengths agree for the hex ids Ghost mints, which is exactly why the
// bug would survive a normal test. The match is therefore
// not served by the primary-key index; a withdrawal is a one-off operator action
// over a project's memories, not a retrieval path, so the scan is the right
// trade. A prefix that matches nothing returns an empty slice and no error —
// what a miss means is the caller's decision, and the callers here report it as
// a miss or as a ref too short to be one.
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
