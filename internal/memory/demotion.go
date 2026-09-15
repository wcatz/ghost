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
// near-duplicate edges instead of 'supersedes' edges: 'related' edges at or
// above threshold, plus Upsert's own 'duplicate' edges.
//
// 'duplicate' edges are exempt from the strength threshold — Upsert already
// gated them at upsertMergeThreshold, so their strength is a Jaccard score,
// not the higher cosine threshold 'related' edges use. Without including them,
// a lexically near-identical save pair survives at full rank unless the
// linker separately wrote a cosine 'related' edge at 0.90.
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
		WHERE invalidated_at IS NULL
		  AND ((relation = 'related' AND strength >= ?) OR relation = 'duplicate')
		  AND source_id IN (%s) AND target_id IN (%s)
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

// demoteResults applies both targeted, window-scoped demotions to a search
// result set: a superseded memory sinks below its superseder, and the
// lower-ranked member of a near-duplicate pair sinks below the other. Both are
// membership-preserving (they only reorder), mirroring GetTopMemories'
// injection ranking so the MCP search path and session injection agree.
func (s *Store) demoteResults(ctx context.Context, results []Memory, p SearchParams) []Memory {
	results = s.demoteSuperseded(ctx, results, p)
	return s.demoteNearDuplicates(ctx, results)
}

// demoteNearDuplicates ranks down the lower-ranked member of each
// near-duplicate pair present in the window. Without it, ghost_memory_search
// returns both members of a pair at full rank even though injection demotes
// one.
func (s *Store) demoteNearDuplicates(ctx context.Context, results []Memory) []Memory {
	if len(results) < 2 {
		return results
	}
	ids := make([]string, len(results))
	pinned := make(map[string]bool, len(results))
	for i, m := range results {
		ids[i] = m.ID
		pinned[m.ID] = m.Pinned
	}
	s.mu.RLock()
	penalty, err := DemotionPenalties(ctx, s.db, ids, pinned, s.demotionThreshold)
	s.mu.RUnlock()
	if err != nil {
		s.logger.Debug("near-duplicate demote: lookup failed", "error", err)
		return results
	}
	if len(penalty) == 0 {
		return results
	}
	return StableDemote(results, func(m Memory) string { return m.ID }, penalty)
}
