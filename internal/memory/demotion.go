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
// linker separately wrote a cosine 'related' edge at 0.90. Scope-conflicting
// endpoint pairs are ignored even when a legacy or manual edge already exists;
// their claims belong to different environments and must not demote one another.
//
// ids order encodes rank (index 0 = highest-ranked). For every 'related' pair
// found, the lower-ranked ID's penalty is incremented — unless that ID is
// PROTECTED and the other isn't, in which case the unprotected one is penalized
// instead regardless of rank, since protection is an explicit user signal to
// keep a memory visible.
//
// The map is called protected rather than pinned because that is what it holds:
// a pin, or a `persistent` tier. Both are the user saying "keep this where it
// is", and the near-duplicate demotion is the one place left that sinks a memory
// every other pass has been taught to leave alone — a keep-forever row that
// consolidation and resolve spare, and that a stale 'duplicate' edge from before
// the tier was declared then demotes on every search, is a protection with a
// hole in it. Callers build the map from the hydrated rows they already hold
// (m.Pinned || RetentionExempt(m)); nothing in this file reads the column
// itself, because these functions are handed ids, not rows. No locking: callers that need Store's s.mu.RLock
// (i.e. GetTopMemories) take it themselves around the call, same as every
// other Store method taking a raw SQL read handle.
func DemotionPenalties(ctx context.Context, db Queryer, ids []string, protected map[string]bool, threshold float64) (map[string]int, error) {
	penalty, err := nearDuplicateVerdicts(ctx, db, ids, protected, threshold, nil)
	if err != nil {
		return nil, err
	}
	return penalty, nil
}

// nearDuplicateVerdicts is DemotionPenalties' edge read with the counterpart
// kept, for the same reason as supersedeVerdicts: one read, both facts, so the
// id explain names is the id the demotion chose rather than a second read's idea
// of it.
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

// nearDuplicatePenaltyRows is the near-duplicate edge set as one loser/winner
// verdict per edge, with the scope exemption and the rank and pinning rules
// applied. It is the one place that decides which member of a pair loses, so the
// penalty a caller demotes by and the ids explain attributes that demotion to
// cannot come from two different reads: a re-derived attribution could name a
// different loser than the demotion chose, and the explanation would then
// contradict the order it is explaining.
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
			loser = winner
		}
		pairs = append(pairs, demotionPairs{loser: loser, winner: winner})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("demotion penalties: %w", err)
	}
	return pairs, nil
}

// demotionPairs is the per-edge outcome both near-duplicate statements agree on:
// the id that loses and the id it lost to.
type demotionPairs struct {
	loser, winner string
}

// demotionVerdicts folds the per-edge outcomes into a penalty count per loser and
// the counterpart ids behind it. It is the shared tail of DemotionPenalties and
// its attribution form, so "which member of a pair loses" is decided once.
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

// Status demotion: multiplicative factors on a candidate's fused score, as
// opposed to the window-scoped penalty counts below. Both are demotion, and
// both live here so the ranking seam (fuseCandidatePool) and explain mode
// (ExplainSearchScoped) read the same constants through one function.
const (
	// resolvedDemotionFactor halves a resolved row: the resolve verdict means
	// "stop surfacing this first", not "forget it". With RRF k=60 every fused
	// score sits in a band of a few thousandths — a rank-1 two-leg hit is
	// 0.3/61 + 0.7/61 ≈ 0.0164 — so halving lands the row at ≈ 0.0082, under
	// the 0.7/80 ≈ 0.0088 floor of the deepest vector-leg row in a default
	// window: effectively below the live pool rather than below one
	// comparable neighbour, and still the answer when nothing live matched.
	resolvedDemotionFactor = 0.5
	// globalDemotionFactor halves a _global row in a project-scoped search:
	// shared rows enter every project's legs, so without this they pad a
	// project's results ahead of the project's own memories.
	globalDemotionFactor = 0.5
)

// statusDemotionFactor is the single decision on how far a candidate sinks for
// its status rather than its match: a resolved row, or a _global row when a
// specific project is being searched. Ranking applies it to the fused score
// inside fuseCandidatePool — before the cut, so it decides membership as well
// as order — and explain mode reports the same number per row, so the factor
// shown is the factor that ranked.
//
// It never removes a row from the candidate pool: every branch only scales a
// score, which is what keeps resolved memories searchable and keeps _global
// rows findable from a project. One admission rule downstream is status-aware
// as well — selectWindow's keyword reservation declines rows whose factor is
// below 1, because reservation reads raw FTS rank and would otherwise hand a
// demoted row back the window slot the factor just took from it — so a
// demoted keyword-only hit makes the window cut on its demoted score rather
// than on the reservation, and comes back when the window has room or it
// clears that cut with no reserved keyword hit waiting to evict it (eviction
// is score-blind, and a demoted row is never reserved). Nothing in this
// function removes a row; it only multiplies.
func statusDemotionFactor(resolved bool, rowProjectID, searchProjectID string) float64 {
	factor := 1.0
	if resolved {
		factor *= resolvedDemotionFactor
	}
	// Empty means a cross-project search: there is no project whose own
	// memories a shared row could be padding, so nothing to demote. Searching
	// _global itself has the same symmetry — a project must not demote its
	// own rows.
	if rowProjectID == "_global" && searchProjectID != "" && searchProjectID != "_global" {
		factor *= globalDemotionFactor
	}
	return factor
}

// demoteStatus scales every fused candidate's score by statusDemotionFactor.
// fuseCandidatePool calls it just before its own sort, so the demoted score is
// what the window cut, the score map, decayRank's base and explain's reported
// rank all read — one multiplication point rather than a second ranking path.
func demoteStatus(pool []*hybridCandidate, p SearchParams) {
	for _, c := range pool {
		factor := statusDemotionFactor(c.resolved, c.projectID, p.ProjectID)
		if t := p.trace.row(c.id); t != nil {
			// The base the legs contributed, with the factor applied to it. The
			// product is not recorded: it is exactly Base × StatusFactor, and the
			// payload note tells a reader to perform that multiplication. Two
			// copies of one product is one more thing that can disagree with its
			// own factors.
			t.Base = c.score
			t.StatusFactor = factor
			t.RowProject = c.projectID
			// A cross-project search has no bucket of its own, so every row
			// matches it by definition — the same rule BucketUnexpected states
			// for the assembler, applied where the project is actually known.
			t.ProjectMatch = p.ProjectID == "" || c.projectID == p.ProjectID
		}
		if factor != 1.0 {
			c.score *= factor
		}
	}
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

// SupersedePenalties returns a penalty count per candidate ID for 'supersedes'
// edges whose BOTH endpoints are in ids (same window rule as SupersedesWithin):
// the superseded ID's penalty is incremented once per present superseder, so a
// memory with no co-present superseder keeps penalty 0. Batched-lookup shape
// mirrors DemotionPenalties so injection paths (GetTopMemories, the session
// hook) can share one query with the search path's demoteSuperseded. No
// locking: same contract as DemotionPenalties — callers holding Store's
// s.mu.RLock (GetTopMemories) pass its read handle; the hook passes its own
// read-only handle.
//
// A scope-conflicting endpoint pair is exempt, for the same reason it is exempt
// in DemotionPenalties and no different one: the edge asserts that one claim
// replaced another, and a memory that names environment=production and one that
// names environment=development are two claims about two places however
// similar their sentences read. internal/supersede refuses to write such an
// edge, but this is the only place that can stop one already in the store — a
// 'supersedes' link written before that rule existed, or by a manual
// CreateLink — from sinking a production answer behind a development one. The
// edge is left in the graph; scope exempts it from ranking rather than deleting
// it.
//
// A `persistent` target is exempt for the same reason it is exempt from the
// classify pass: the exemption has to reach the demotion, or an edge written
// before the row was declared keep-forever sinks it on the next search anyway.
// The edge stays — withdrawing a claim is the user's own explicit move — and it
// stops ranking the row it names.
func SupersedePenalties(ctx context.Context, db Queryer, ids []string) (map[string]int, error) {
	penalty, err := supersedeVerdicts(ctx, db, ids, nil)
	if err != nil {
		return nil, err
	}
	return penalty, nil
}

// supersedeVerdicts is SupersedePenalties' edge read with the counterpart kept,
// so a caller that wants to REPORT the demotion gets the count and the ids from
// one statement. The trace is written here rather than by a second read because
// the ranking path's own lookup already happened: a separate attribution query
// would be a second chance to disagree with it, on the store's single
// connection, inside a snapshot a concurrent writer is already locked out of.
func supersedeVerdicts(ctx context.Context, db Queryer, ids []string, tr *searchTrace) (map[string]int, error) {
	rows, err := supersedePenaltyRows(ctx, db, ids)
	if err != nil {
		return nil, err
	}
	penalty := make(map[string]int, len(rows))
	for target, sources := range rows {
		penalty[target] = len(sources)
		// Sorted so the rendered list is stable: the edges arrive in whatever
		// order the index walk produced, and a list that reshuffles between two
		// identical explanations is not a diagnosis anyone can follow. The count
		// is unaffected — a chain of superseders sinks the same row once each
		// either way.
		sort.Strings(sources)
		if t := tr.row(target); t != nil {
			t.SupersedePenalty = penalty[target]
			t.SupersededBy = sources
		}
	}
	return penalty, nil
}

// supersedePenaltyRows is the 'supersedes' edge set within ids as target ->
// present superseders, with the scope exemption applied. It is the one place
// that decides which endpoint a 'supersedes' edge sinks, so the count a caller
// demotes by and the ids explain attributes that demotion to cannot come from
// two different reads.
func supersedePenaltyRows(ctx context.Context, db Queryer, ids []string) (map[string][]string, error) {
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
		SELECT l.source_id, l.target_id, source_mem.scope, target_mem.scope, target_mem.retention
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
		var targetRetention string
		if err := rows.Scan(&src, &tgt, &sourceScope, &targetScope, &targetRetention); err != nil {
			return nil, fmt.Errorf("supersede penalties: %w", err)
		}
		if ScopesConflict(parseScope(sourceScope), parseScope(targetScope)) {
			continue
		}
		// src supersedes tgt: sink the superseded side once per edge. A target
		// the user declared keep-forever is not sunk by an edge at all — see
		// the function comment.
		if targetRetention == RetentionPersistent {
			continue
		}
		// src supersedes tgt: sink the superseded side once per edge.
		// The query already restricted both endpoints to ids, so src is
		// present by construction (same rule demoteSuperseded applies).
		against[tgt] = append(against[tgt], src)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("supersede penalties: %w", err)
	}
	return against, nil
}

// demoteResults applies both targeted, window-scoped demotions to a search
// result set: a superseded memory sinks below its superseder, and the
// lower-ranked member of a near-duplicate pair sinks below the other. Both are
// membership-preserving (they only reorder), mirroring GetTopMemories'
// injection ranking so the MCP search path and session injection agree.
func (s *Store) demoteResults(ctx context.Context, results []Memory, p SearchParams) []Memory {
	results = s.demoteSuperseded(ctx, results, p)
	return s.demoteNearDuplicates(ctx, results, p)
}

// demoteNearDuplicates ranks down the lower-ranked member of each
// near-duplicate pair present in the window. Without it, ghost_memory_search
// returns both members of a pair at full rank even though injection demotes
// one.
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
	penalty, err := nearDuplicateVerdicts(ctx, s.queryDB(), ids, protected, s.demotionThreshold, p.trace)
	s.mu.RUnlock()
	if err != nil {
		s.logger.Debug("near-duplicate demote: lookup failed", "error", err)
		return results
	}
	if len(penalty) == 0 {
		return results
	}
	// As with supersedes, the counterpart ids come from the same edge read the
	// penalty was decided on: which member of a cluster loses is decided by
	// position in the window, so a separate attribution read would be a second
	// chance to name a different loser than the demotion chose.
	return StableDemote(results, func(m Memory) string { return m.ID }, penalty)
}
