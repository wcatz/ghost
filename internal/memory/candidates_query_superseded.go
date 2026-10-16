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
//
// The name is for the assembler's marker ONLY. A live edge switches the lexical
// guess (lexicalReplacements) off for its pair on a query read as on a passive
// one, and resolve never retires a convention or a preference, so without the
// name a search answer would say less about a replaced row once the lifecycle
// had linked it than it said before. Nothing here reorders, demotes or removes a
// row: the query demotion read its own edges earlier and the order is final.
//
// rows is the edge scope (edgeScopeIDs), the ids the lexical guess is asked
// over. A failed read is logged and names nothing, which leaves every line as it
// rendered before the name existed.
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
