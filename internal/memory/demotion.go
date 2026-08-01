package memory

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// DefaultDemotionThreshold is the near-duplicate demotion cutoff used until
// config overrides it (see Store.SetDemotionThreshold and
// internal/mcpinit/hook.go's own fallback for the config-less hook path).
const DefaultDemotionThreshold = 0.90

// DemotionPenalties returns a penalty count per candidate ID, mirroring
// SupersedesWithin's batched-lookup shape (internal/memory/links.go) but over
// 'related' edges at or above threshold instead of 'supersedes' edges.
//
// ids order encodes rank (index 0 = highest-ranked). For every 'related' pair
// found, the lower-ranked ID's penalty is incremented — unless that ID is
// pinned and the other isn't, in which case the unpinned one is penalized
// instead regardless of rank, since pinning is an explicit user signal to
// keep a memory visible. No locking: callers that need Store's s.mu.RLock
// (i.e. GetTopMemories) take it themselves around the call, same as every
// other Store method taking a raw *sql.DB.
func DemotionPenalties(ctx context.Context, db *sql.DB, ids []string, pinned map[string]bool, threshold float64) (map[string]int, error) {
	if len(ids) < 2 {
		return nil, nil
	}

	rank := make(map[string]int, len(ids))
	ph := make([]string, len(ids))
	idArgs := make([]interface{}, len(ids))
	for i, id := range ids {
		rank[id] = i
		ph[i] = "?"
		idArgs[i] = id
	}
	list := strings.Join(ph, ",")

	args := make([]interface{}, 0, 1+len(ids)*2)
	args = append(args, threshold)
	args = append(args, idArgs...)
	args = append(args, idArgs...)

	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT source_id, target_id FROM memory_links
		WHERE relation = 'related' AND invalidated_at IS NULL
		  AND strength >= ? AND source_id IN (%s) AND target_id IN (%s)
	`, list, list), args...)
	if err != nil {
		return nil, fmt.Errorf("demotion penalties: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	penalty := make(map[string]int, len(ids))
	for rows.Next() {
		var a, b string
		if err := rows.Scan(&a, &b); err != nil {
			return nil, fmt.Errorf("demotion penalties: %w", err)
		}
		loser, winner := a, b
		if rank[b] > rank[a] {
			loser, winner = b, a
		}
		if pinned[loser] && !pinned[winner] {
			loser = winner
		}
		penalty[loser]++
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("demotion penalties: %w", err)
	}
	return penalty, nil
}

// StableDemote reorders items ascending by penalty (ties keep existing
// relative order), mirroring demoteSuperseded's sort.SliceStable pattern
// (internal/memory/vector.go) generically over any item shape that can name
// its own ID via the id accessor.
func StableDemote[T any](items []T, id func(T) string, penalty map[string]int) []T {
	sort.SliceStable(items, func(i, j int) bool {
		return penalty[id(items[i])] < penalty[id(items[j])]
	})
	return items
}
