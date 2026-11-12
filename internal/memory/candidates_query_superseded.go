package memory

import (
	"context"
	"sort"
)

// one, and resolve never retires a convention or a preference, so without the
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
