package memory

import (
	"context"
	"sort"
)

// querySupersededBy names, for each row of a QUERY read, the rows of the same
// read that supersede it through a live `supersedes` edge, sorted. It is the
// query read's counterpart of passiveSupersededBy and reads the same rules:
// supersedePenaltyRows with no persistent-retention exemption, so a live edge
// only (a withdrawn edge names nothing), an edge between conflicting scopes
// names nothing, and a keep-forever target is named although the demotion never
// sinks it.
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
