package memory

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// DefaultDemotionThreshold is the near-duplicate demotion cutoff used until

const DefaultDemotionThreshold = 0.90

// DemotionPenalties returns a penalty count per candidate ID, mirroring

func DemotionPenalties(ctx context.Context, db Queryer, ids []string, protected map[string]bool, threshold float64) (map[string]int, error) {
	penalty, err := nearDuplicateVerdicts(ctx, db, ids, protected, threshold, nil)
	if err != nil {
		return nil, err
	}
	return penalty, nil
}

func nearDuplicateVerdicts(ctx context.Context, db Queryer, ids []string, protected map[string]bool, threshold float64, tr *searchTrace) (map[string]int, error) {
	pairs, err := nearDuplicatePenaltyRows(ctx, db, ids, protected, threshold)
	if err != nil {
		return nil, err
	}
	penalty, against := demotionVerdicts(pairs)
	for _, id := range ids {
		if t := tr.row(id); t != nil {
			t.NearDuplicatePenalty = penalty[id]
			t.NearDuplicateOf = against[id]
		}
	}
	return penalty, nil
}

func nearDuplicatePenaltyRows(ctx context.Context, db Queryer, ids []string, protected map[string]bool, threshold float64) ([]demotionPairs, error) {
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
		SELECT l.source_id, l.target_id, source_mem.scope, target_mem.scope
		FROM memory_links l
		JOIN memories source_mem ON source_mem.id = l.source_id
		JOIN memories target_mem ON target_mem.id = l.target_id
		WHERE l.invalidated_at IS NULL
		  AND ((l.relation = 'related' AND l.strength >= ?) OR l.relation = 'duplicate')
		  AND l.source_id IN (%s) AND l.target_id IN (%s)
	`, list, list), args...)
	if err != nil {
		return nil, fmt.Errorf("demotion penalties: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var pairs []demotionPairs
	for rows.Next() {
		var a, b string
		var sourceScope, targetScope sql.NullString
		if err := rows.Scan(&a, &b, &sourceScope, &targetScope); err != nil {
			return nil, fmt.Errorf("demotion penalties: %w", err)
		}
		if ScopesConflict(parseScope(sourceScope), parseScope(targetScope)) {
			continue
		}
		loser, winner := a, b
		if rank[b] > rank[a] {
			loser, winner = b, a
		}
		if protected[loser] && !protected[winner] {

			loser, winner = winner, loser
		}
		pairs = append(pairs, demotionPairs{loser: loser, winner: winner})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("demotion penalties: %w", err)
	}
	return pairs, nil
}

func earlierRowLoses(pairs []demotionPairs, rows []Memory, window int, protected map[string]bool, explicit []LinkEdge) []demotionPairs {
	if len(pairs) == 0 {
		return pairs
	}
	related := make(map[[2]string]bool, 2*len(explicit))
	for _, e := range explicit {
		related[[2]string{e.From, e.To}] = true
		related[[2]string{e.To, e.From}] = true
	}
	created := make(map[string]time.Time, len(rows))
	inTail := make(map[string]bool, len(rows))
	for i, m := range rows {
		if at, ok := ParseStamp(m.CreatedAt); ok {
			created[m.ID] = at
		}
		if i >= window {
			inTail[m.ID] = true
		}
	}
	out := make([]demotionPairs, 0, len(pairs))
	for _, pr := range pairs {
		lc, lok := created[pr.loser]
		wc, wok := created[pr.winner]
		if lok && wok && lc.After(wc) && (!protected[pr.winner] || protected[pr.loser]) && (!inTail[pr.loser] || inTail[pr.winner]) && !related[[2]string{pr.loser, pr.winner}] {
			pr.loser, pr.winner = pr.winner, pr.loser
			pr.flipped = true
		}
		out = append(out, pr)
	}
	return out
}

type demotionPairs struct {
	loser, winner string

	flipped bool
}

func vetoLosers(pairs []demotionPairs, vetoed func(demotionPairs) bool) []demotionPairs {
	kept := make([]demotionPairs, 0, len(pairs))
	for _, pr := range pairs {
		if !vetoed(pr) {
			kept = append(kept, pr)
			continue
		}
		if !pr.flipped {
			continue
		}
		rank := demotionPairs{loser: pr.winner, winner: pr.loser}
		if !vetoed(rank) {
			kept = append(kept, rank)
		}
	}
	return kept
}

func contradictingIDs(ctx context.Context, db Queryer, ids []string) (map[string]bool, error) {
	if len(ids) < 2 {
		return nil, nil
	}
	ph := make([]string, len(ids))
	args := make([]interface{}, 0, len(ids)*2)
	for i, id := range ids {
		ph[i] = "?"
		args = append(args, id)
	}
	for _, id := range ids {
		args = append(args, id)
	}
	list := strings.Join(ph, ",")

	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT l.source_id, l.target_id
		FROM memory_links l
		WHERE l.relation = 'contradicts' AND l.invalidated_at IS NULL
		  AND l.source_id IN (%s) AND l.target_id IN (%s)
	`, list, list), args...)
	if err != nil {
		return nil, fmt.Errorf("contradicting pairs: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var out map[string]bool
	for rows.Next() {
		var a, b string
		if err := rows.Scan(&a, &b); err != nil {
			return nil, fmt.Errorf("contradicting pairs: %w", err)
		}
		if out == nil {
			out = make(map[string]bool, 2)
		}
		out[a], out[b] = true, true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("contradicting pairs: %w", err)
	}
	return out, nil
}

func demotionVerdicts(pairs []demotionPairs) (map[string]int, map[string][]string) {
	penalty := make(map[string]int)
	against := make(map[string][]string)
	for _, pr := range pairs {
		penalty[pr.loser]++
		against[pr.loser] = append(against[pr.loser], pr.winner)
	}
	for id := range against {
		sort.Strings(against[id])
	}
	return penalty, against
}

const (
	resolvedDemotionFactor = 0.5

	globalDemotionFactor = 0.5
)

func statusDemotionFactor(resolved bool, rowProjectID, searchProjectID string) float64 {
	factor := 1.0
	if resolved {
		factor *= resolvedDemotionFactor
	}

	if rowProjectID == "_global" && searchProjectID != "" && searchProjectID != "_global" {
		factor *= globalDemotionFactor
	}
	return factor
}

func demoteStatus(pool []*hybridCandidate, p SearchParams) {
	for _, c := range pool {
		factor := statusDemotionFactor(c.resolved, c.projectID, p.ProjectID)
		if t := p.trace.row(c.id); t != nil {

			t.Base = c.score
			t.StatusFactor = factor
			t.RowProject = c.projectID

			t.ProjectMatch = p.ProjectID == "" || c.projectID == p.ProjectID
		}
		if factor != 1.0 {
			c.score *= factor
		}
	}
}

// StableDemote reorders items ascending by penalty (ties keep existing

func StableDemote[T any](items []T, id func(T) string, penalty map[string]int) []T {
	sort.SliceStable(items, func(i, j int) bool {
		return penalty[id(items[i])] < penalty[id(items[j])]
	})
	return items
}

// SupersedePenalties returns a penalty count per candidate ID for 'supersedes'

func SupersedePenalties(ctx context.Context, db Queryer, ids []string, protected map[string]bool) (map[string]int, error) {
	penalty, err := supersedeVerdicts(ctx, db, ids, nil, protected)
	if err != nil {
		return nil, err
	}
	return penalty, nil
}

func supersedeVerdicts(ctx context.Context, db Queryer, ids []string, tr *searchTrace, protected map[string]bool) (map[string]int, error) {
	rows, err := supersedePenaltyRows(ctx, db, ids, protected)
	if err != nil {
		return nil, err
	}
	penalty := make(map[string]int, len(rows))
	for target, sources := range rows {
		penalty[target] = len(sources)

		sort.Strings(sources)
		if t := tr.row(target); t != nil {
			t.SupersedePenalty = penalty[target]
			t.SupersededBy = sources
		}
	}
	return penalty, nil
}

func supersedePenaltyRows(ctx context.Context, db Queryer, ids []string, protected map[string]bool) (map[string][]string, error) {
	if len(ids) < 2 {
		return nil, nil
	}

	ph := make([]string, len(ids))
	args := make([]interface{}, 0, len(ids)*2)
	for i, id := range ids {
		ph[i] = "?"
		args = append(args, id)
	}
	for _, id := range ids {
		args = append(args, id)
	}
	list := strings.Join(ph, ",")

	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT l.source_id, l.target_id, source_mem.scope, target_mem.scope
		FROM memory_links l
		JOIN memories source_mem ON source_mem.id = l.source_id
		JOIN memories target_mem ON target_mem.id = l.target_id
		WHERE l.relation = 'supersedes' AND l.invalidated_at IS NULL
		  AND l.source_id IN (%s) AND l.target_id IN (%s)
	`, list, list), args...)
	if err != nil {
		return nil, fmt.Errorf("supersede penalties: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	against := make(map[string][]string, len(ids))
	for rows.Next() {
		var src, tgt string
		var sourceScope, targetScope sql.NullString
		if err := rows.Scan(&src, &tgt, &sourceScope, &targetScope); err != nil {
			return nil, fmt.Errorf("supersede penalties: %w", err)
		}
		if ScopesConflict(parseScope(sourceScope), parseScope(targetScope)) {
			continue
		}

		if protected[tgt] {
			continue
		}

		against[tgt] = append(against[tgt], src)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("supersede penalties: %w", err)
	}
	return against, nil
}

func (s *Store) demoteResults(ctx context.Context, results []Memory, p SearchParams) []Memory {
	results = s.demoteSuperseded(ctx, results, p)
	return s.demoteNearDuplicates(ctx, results, p)
}

func (s *Store) demoteNearDuplicates(ctx context.Context, results []Memory, p SearchParams) []Memory {
	if len(results) < 2 {
		return results
	}
	ids := make([]string, len(results))
	protected := make(map[string]bool, len(results))
	for i, m := range results {
		ids[i] = m.ID
		protected[m.ID] = m.Pinned || RetentionExempt(m)
	}
	s.mu.RLock()
	pairs, err := nearDuplicatePenaltyRows(ctx, s.queryDB(), ids, protected, s.demotionThreshold)
	var exemptErr error
	if err == nil {
		pairs, exemptErr = exemptReplacementPairs(ctx, s.queryDB(), pairs, results, time.Now().UTC())
	}
	s.mu.RUnlock()
	if err != nil {
		s.logger.Debug("near-duplicate demote: lookup failed", "error", err)
		return results
	}
	if exemptErr != nil {
		s.logger.Debug("near-duplicate demote: replacement pair lookup failed, demoting without the exemption", "error", exemptErr)
	}
	penalty, against := demotionVerdicts(pairs)
	for _, id := range ids {
		if t := p.trace.row(id); t != nil {
			t.NearDuplicatePenalty = penalty[id]
			t.NearDuplicateOf = against[id]
		}
	}
	if len(penalty) == 0 {
		return results
	}

	return StableDemote(results, func(m Memory) string { return m.ID }, penalty)
}

func (s *Store) dropNearDuplicates(ctx context.Context, results []Memory, p SearchParams, now time.Time) (kept, removed []Memory, lostTo, standIns map[string][]string) {
	return s.dropNearDuplicatesOver(ctx, results, len(results), p, now)
}

func (s *Store) dropNearDuplicatesOver(ctx context.Context, results []Memory, window int, p SearchParams, now time.Time) (kept, removed []Memory, lostTo, standIns map[string][]string) {

	lostTo = make(map[string][]string)
	if len(results) < 2 {
		return results, nil, lostTo, nil
	}
	ids := make([]string, len(results))
	protected := make(map[string]bool, len(results))
	for i, m := range results {
		ids[i] = m.ID
		protected[m.ID] = m.Pinned || RetentionExempt(m)
	}
	s.mu.RLock()
	pairs, err := nearDuplicatePenaltyRows(ctx, s.queryDB(), ids, protected, s.demotionThreshold)
	if err == nil && len(pairs) > 0 {
		var explicit []LinkEdge
		explicit, err = explicitEdgesWithin(ctx, s.queryDB(), ids)
		if err == nil {
			pairs = earlierRowLoses(pairs, results, window, protected, explicit)
		}
	}
	if err == nil && len(pairs) > 0 {
		var contradicted map[string]bool
		contradicted, err = contradictingIDs(ctx, s.queryDB(), ids)
		if err == nil && len(contradicted) > 0 {
			pairs = vetoLosers(pairs, func(pr demotionPairs) bool { return contradicted[pr.loser] })
		}
	}
	if err == nil && len(pairs) > 0 {
		pairs, err = replacementRemovalVetoes(ctx, s.queryDB(), pairs, results, now)
	}
	s.mu.RUnlock()
	if err != nil {
		s.logger.Warn("near-duplicate drop: lookup failed", "error", err)
		return results, nil, lostTo, nil
	}
	penalty, against := demotionVerdicts(pairs)

	for _, id := range ids {
		if t := p.trace.row(id); t != nil {
			t.NearDuplicatePenalty = penalty[id]
			t.NearDuplicateOf = against[id]
		}
	}
	if len(penalty) == 0 {
		return results, nil, lostTo, nil
	}
	kept = make([]Memory, 0, len(results))
	for _, m := range results {
		if penalty[m.ID] > 0 {
			removed = append(removed, m)
			continue
		}
		kept = append(kept, m)
	}
	lostTo = against
	for _, pr := range pairs {
		if !pr.flipped {
			continue
		}
		if standIns == nil {
			standIns = make(map[string][]string)
		}
		standIns[pr.loser] = append(standIns[pr.loser], pr.winner)
	}
	for id := range standIns {
		sort.Strings(standIns[id])
	}
	return kept, removed, lostTo, standIns
}
