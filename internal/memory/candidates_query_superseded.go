package memory

import (
	"context"
	"sort"
)

// querySupersededBy names a query row's live superseders, sorted. It follows
// supersedePenaltyRows with no retention exemption: withdrawn edges and
// conflicting scopes name nothing, and a keep-forever target is still named.
func (s *Store) querySupersededBy(ctx context.Context, rows []Candidate) map[string][]string {
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	against, err := supersedePenaltyRows(ctx, s.queryDB(), ids, nil)
	if err != nil {
		s.logger.Warn("candidates: query supersede lookup failed", "error", err)
		return nil
	}
	out := make(map[string][]string, len(against))
	for target, sources := range against {
		sorted := append([]string(nil), sources...)
		sort.Strings(sorted)
		out[target] = sorted
	}
	return out
}
