package memory

// Passive retrieval: the session-start path's selection, on the retriever seam.
//
// The policies are SPECIFICATION of what the two session-start loaders did, not
// a redesign, and the reason they live here rather than in a caller is the one
// that makes the assembler worth having: selection, filtering and rendering can
// only be the same code if selection is something the assembler can ask for.
//
// What a passive retrieval is not: a search with an empty query. No leg runs, so
// there is no rank, no cosine and no relevance order. Ranking is importance
// times a category-and-age decay, with pinned exempted, and each bucket states
// its own order because the two genuinely differ.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// passiveOrders are the two ranking orders a bucket policy may name. The
// constant is validated rather than interpolated, because it is a fragment
// spliced into an ORDER BY.
const (
	// OrderDecay is the composite score, then importance, created_at and id — the
	// order GetTopMemories uses, and the one the project bucket ships.
	OrderDecay = "decay"
	// OrderPinnedImportanceUpdated is the `_global` bucket's own order: pinned
	// first, then importance, then most-recently-updated. It is a separate policy
	// rather than a variation, because a global preference is a standing
	// instruction and a project memory is a note with an age.
	OrderPinnedImportanceUpdated = "pinned_importance_updated"
)

// stampLayoutForSQL is the layout a bound instant is written in, matching the
// layout the writers stamp created_at and updated_at in so julianday() parses
// it.
const stampLayoutForSQL = "2006-01-02 15:04:05"

// maxPassiveOverFetch is the ceiling on one bucket's passive window.
//
// It is a refusal rather than a clamp because the two disagree about what the
// caller asked for, and only one of them is honest: a caller that states 5000
// and is silently given 200 has been answered with a different question. The
// ceiling is well above what the session-start policies need (45 and 16) and
// above the query path's own 100-row window, because a passive window is not
// comparable to a fusion window: it is read, ordered and demoted in full, with
// no leg to fuse and nothing to discard behind it.
const maxPassiveOverFetch = 200

// validatePassivePolicies refuses a passive request the store cannot serve
// honestly, before any query runs. Each refusal is a shape that would otherwise
// be answered with a set nobody can read:
//
//   - no policy at all: nothing to retrieve by, and an empty set would read as a
//     store holding nothing.
//   - a policy naming no bucket: it would fetch nothing under that name, which is
//     the same empty-set lie wearing a different hat.
//   - a repeated bucket: the rows are fetched once per policy and concatenated,
//     so the set would carry every row of that bucket TWICE — a duplicate id
//     reaching the assembler, which then reports it as two rows that happened to
//     rank equally. assemble refuses the same shape at its own layer; the store
//     refuses it too because it is reachable directly.
//   - a window beyond the ceiling, or no window at all: this path runs at every
//     session start, so both are a store scan wearing a number.
func validatePassivePolicies(policies []SlicePolicy) error {
	if len(policies) == 0 {
		return ErrPassiveUnsupported
	}
	seen := make(map[string]bool, len(policies))
	for _, pol := range policies {
		if pol.Bucket == "" {
			return errors.New("candidates: a passive policy names no bucket, so it would fetch nothing and read as an empty bucket")
		}
		if seen[pol.Bucket] {
			return fmt.Errorf("candidates: two passive policies name bucket %q, so its rows would be fetched twice and every "+
				"one of them returned twice; one policy per bucket", pol.Bucket)
		}
		seen[pol.Bucket] = true
		if pol.OverFetch <= 0 {
			return fmt.Errorf("candidates: passive policy for bucket %q states no over-fetch, so its window would be the whole store; "+
				"this path runs at every session start", pol.Bucket)
		}
		if pol.OverFetch > maxPassiveOverFetch {
			return fmt.Errorf("candidates: passive policy for bucket %q asks for %d rows; the ceiling is %d. A passive window is "+
				"read, ordered and demoted in full with no leg to discard behind it, so it is clamped far lower than a fusion window",
				pol.Bucket, pol.OverFetch, maxPassiveOverFetch)
		}
		switch pol.Order {
		case OrderDecay, OrderPinnedImportanceUpdated, "":
		default:
			return fmt.Errorf("candidates: passive policy for bucket %q names unknown order %q", pol.Bucket, pol.Order)
		}
	}
	return nil
}

// candidatesPassive serves a request with no query. Every policy is a separate
// read: they disagree about order, over-fetch, selection and the near-duplicate
// policy, and running one statement for all of them would mean choosing one
// bucket's policy for the other.
//
// The returned set is WIDENED, and that is the property the whole seam exists
// for. A policy's selection REORDERS its window rather than truncating it — the
// two-pass floor lifts the reserved rows to the front, the demotions push losers
// down — so the whole window comes back, ordered, and a later stage that drops a
// row (validity, scope) can still reach the row behind it. The assembler's cap
// then closes the window, which is the only thing that truncates.
func (s *Store) candidatesPassive(ctx context.Context, req CandidateRequest, set *CandidateSet) (*CandidateSet, error) {
	// Resolved once, before any bucket runs, because it decides both the SELECT
	// list and the tier half of the ORDER BY. See passiveColumnsFor.
	cols, err := passiveColumnsFor(s)
	if err != nil {
		return nil, err
	}
	var rows []Candidate
	for _, pol := range req.Passive {
		fetched, err := s.passiveBucket(ctx, req, pol, cols)
		if err != nil {
			return nil, err
		}
		rows = append(rows, fetched...)
	}
	set.Rows = rows
	// Widened is deliberately LEFT FALSE, which is what it means here rather than
	// an omission. The field reports that the set is LARGER than the requested
	// window — the query path's fusion window, behind which the discarded tail
	// still travels. A passive fetch has no such thing: its whole window is read,
	// ordered, demoted and returned (the selection's leftovers included, as the
	// backfill supply), so the set is never larger than what was asked for. The
	// backfill the assembler needs is therefore carried by the ORDER — the
	// selected rows first — not by a widened count, and nothing reads Widened on
	// this path. Writing the opposite of the field's meaning here would be the
	// worst of the three options.

	// The edges and the evidence counts are read on the same snapshot as the rows
	// above, for the reason the query path reads them there: a count taken after
	// the transaction closed could describe a save these rows predate, and the
	// trace would then report support for a state of the corpus it did not
	// retrieve.
	scope := edgeScopeIDs(rows, len(rows))
	edges, status := s.loadCandidateEdges(ctx, scope)
	set.Edges, set.EdgesStatus = edges, status

	// The evidence counts, or ZERO of them on a store with no provenance table.
	// Nothing ranks on these counts — stage 4's multiplier is pinned at 1.0 — so the
	// zero is the honest answer for a store that has never recorded an
	// observation, and it is the same answer a populated-but-empty table gives. The
	// error is NOT degraded the same way: on a store that HAS the table and cannot
	// read it, a zero would be a false claim that no memory is supported, which is
	// the one thing this read must never produce.
	counts := map[string]EvidenceCounts{}
	if cols.HasProvenance {
		counts, err = evidenceCountsFor(ctx, s.queryDB(), scope)
		if err != nil {
			return nil, fmt.Errorf("candidates: evidence counts: %w", err)
		}
	} else {
		s.logger.Debug("candidates: passive read skipped the evidence counts: the store predates memory_provenance")
	}
	for i := range rows {
		rows[i].Evidence = counts[rows[i].ID]
	}
	return set, nil
}

// passiveBucket runs one policy: the fetch, the selection, the demotions, and
// the selected rows followed by the rest of the window.
//
// The store's shape is resolved ONCE for the whole passive request rather than
// per bucket: it costs a PRAGMA, and a session start with two buckets would pay
// for it twice. It is passed down rather than re-read so every bucket's SQL, and
// every bucket's reading of what its rows carry, agree about which store they are
// talking to.
func (s *Store) passiveBucket(ctx context.Context, req CandidateRequest, pol SlicePolicy, cols passiveColumns) ([]Candidate, error) {
	query, args := passiveFetchSQL(pol, req, cols)
	rows, err := s.queryDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("candidates: passive fetch for bucket %q: %w", pol.Bucket, err)
	}
	defer func() { _ = rows.Close() }()
	memories, err := scanMemories(rows)
	if err != nil {
		return nil, err
	}
	return s.selectPassive(ctx, memories, pol, req.Now)
}

// passiveFetchSQL builds one policy's read and its bindings TOGETHER, because a
// mismatch between an ORDER BY and its argument list is not an error — it binds
// the clock to the wrong column, and a fully decayed row then reads as a fresh
// one with no complaint anywhere.
//
// The project predicate is the POLICY'S BUCKET, and `CandidateRequest.Mode` does
// not apply here. That is the whole shape of a passive read: one statement per
// bucket, each naming its own project, where the query path folds project and
// `_global` into a single leg with a mode selecting the predicate. A caller that
// wants both states two policies, which is what the assembler's budget slices
// become — and which is why nothing here merges a project row and a global row in
// one result set for the assembler to have to separate.
//
// The bindings are the bucket, then the clock for the decay order only, then the
// limit. The scope predicate contributes none: ScopeMatchesSQL embeds the
// requested values as a quoted JSON literal, so a value holding a quote is
// escaped rather than allowed to end the statement early.
func passiveFetchSQL(pol SlicePolicy, req CandidateRequest, cols passiveColumns) (string, []any) {
	// The scope predicate is applied HERE, in SQL, rather than only in the
	// assembler's stage 3. The over-fetch chooses which rows are read at all, so
	// a row the session excluded must not spend any of the window: filtering it
	// afterwards would fill the window with rows the caller rejected and then cut
	// them, and a window that small reaches a weaker block.
	//
	// GATED on the column existing, which is the same guard the session-start
	// loaders apply and for the same reason: on a store below the scope floor the
	// predicate names a column that is not there, and the whole fetch would fail
	// with `no such column: scope` — where the loaders it replaces render an
	// unscoped block. Suppressing it is correct rather than merely safe: every row
	// in such a store carries no scope, and ScopesConflict over an empty scope has
	// no keys, so an unscoped row never conflicts with a session scope. The rows
	// the block then shows are the ones that store has, labelled without a scope —
	// which is exactly the block it produced before scope was read.
	scopeClause := ""
	if cols.HasScope && len(req.Scope) > 0 {
		scopeClause = " AND " + ScopeMatchesSQL("scope", req.Scope)
	}

	// The `_global` order carries a trailing `id` that the shipped loader's query
	// does not. It is a divergence from the specification and a deliberate one: a
	// tie on (pinned, importance, updated_at) has no defined order, so the loader
	// returned those rows in whatever order SQLite produced — which is stable for
	// a given database file but is not a function of the data. Naming `id` makes
	// the block a function of the store's contents, which is what a golden
	// comparison across machines needs. It can only ever REORDER rows the loader
	// left unordered, never move a row past one it ranked.
	orderBy := "pinned DESC, importance DESC, updated_at DESC, id"
	args := []any{pol.Bucket}
	if pol.Order == "" || pol.Order == OrderDecay {
		// The rank expression takes a BOUND instant rather than julianday('now'),
		// so the window, the decay score derived from it in selectPassive, and
		// the row ages the trace reports are all made against the same clock. The
		// wall-clock form is what the shipped loaders interpolate, and using it
		// here would reintroduce the drift a bound Now exists to remove, at a
		// moment when the two reads can straddle a second boundary.
		rank, clock := decayRankingSQLAt(req.Now, cols.HasTier)
		orderBy = fmt.Sprintf("(%s) DESC, importance DESC, created_at DESC, id", rank)
		args = append(args, clock...)
	}

	// The column list is the shared one every whole-Memory reader selects, rather
	// than a hand-written copy: the retention change added two columns and a copy
	// here would have failed at Scan with a count mismatch, which is a loud failure
	// but still a second statement of what a Memory is. `memoryColumns` is derived
	// from `memoryColumnNames`, which is the single list.
	query := fmt.Sprintf(`
		SELECT %s
		FROM memories
		WHERE project_id = ? AND resolved_at IS NULL%s
		ORDER BY %s
		LIMIT ?`, cols.list, scopeClause, orderBy)
	return query, append(args, pol.OverFetch)
}

// selectPassive applies the policy's selection and demotions, and returns the
// selected rows followed by the rest of the window.
func (s *Store) selectPassive(ctx context.Context, memories []Memory, pol SlicePolicy, now time.Time) ([]Candidate, error) {
	scored := make([]passiveRow, 0, len(memories))
	for _, m := range memories {
		age := ageDays(m.CreatedAt, now)
		// The tier rides the decay rather than being a second multiplication at
		// each call site, so a session-scoped memory reads as session-scoped in
		// the one number that ranks it — the same reason DecayFactor takes it
		// (#709). The SQL order below computes the category-and-age half only, so
		// a tiered row is ordered slightly differently from how it is scored; that
		// is the same split the query path has, and the score is what the two-pass
		// selection ranks on.
		decay := DecayFactor(m.Category, m.Retention, m.Pinned, age)
		scored = append(scored, passiveRow{
			mem:   m,
			age:   age,
			decay: decay,
			score: float64(m.Importance) * decay,
		})
	}
	// The two-pass selection fills a POOL of twice the item cap, which is the
	// shipped shape and is not a detail: a window wider than the cap needs rows
	// behind the cut so a demoted one can be backfilled, and bounding the pool at
	// the cap itself would leave it nothing to trade. Rows the pool excludes are
	// still returned, behind the selection, as the assembler's backfill supply.
	chosen, rest := passiveSelect(scored, pol)
	chosen = s.passiveDemote(ctx, chosen, pol)

	out := make([]Candidate, 0, len(scored))
	for _, r := range append(chosen, rest...) {
		out = append(out, passiveCandidate(r))
	}
	return out, nil
}

// passiveRow is one fetched row with the facts the passive order and the
// two-pass scoring are computed from, so a row is not re-read to be scored.
type passiveRow struct {
	mem   Memory
	age   float64
	decay float64
	score float64
}

// passiveTwoPass returns the window ordered with the behavioral reservation
// first.
//
// The reservation exists because behavioral categories are high-signal notes an
// agent cannot reconstruct by reading source, so a plain score fill can leave
// them all behind. CategoryCaps is what stops one category taking every reserved
// slot: without it a gotcha-heavy corpus fills the floor with gotchas and
// convention, preference and decision never appear at all.
//
// It REORDERS rather than truncates. The rows past the floor's budget stay in
// the set behind the reserved ones, which is what makes the set a usable
// backfill pool for a stage that later drops a reserved row.
// passiveSelect applies the policy's selection and returns the selected rows
// alongside the rows it did not select, in the policy's own order.
//
// Without a two-pass reservation the whole window is selected and the tail is
// empty: there is no selection to leave anything out of.
func passiveSelect(rows []passiveRow, pol SlicePolicy) (chosen, rest []passiveRow) {
	if !pol.TwoPass || pol.BehaviorFloor <= 0 {
		return rows, nil
	}
	poolCap := passivePoolCap(pol, len(rows))
	picked := passiveTwoPass(rows, pol, poolCap)
	// The tail is what the pool left behind, in the policy's order. It is
	// returned UNDEMOTED on purpose: the demotions below are decided over the
	// selected set (that is the window the shipped loaders use), and applying them
	// to rows outside it would invent a decision no policy made. The tail exists
	// so a later stage that drops a selected row can be backfilled rather than
	// leaving a hole.
	for _, r := range rows {
		if !containsPassiveRow(picked, r.mem.ID) {
			rest = append(rest, r)
		}
	}
	return picked, rest
}

// passivePoolCap is how many rows the two-pass selection admits: twice the item
// cap, never more than the window, and never fewer than the behavioral floor it
// has to honour.
func passivePoolCap(pol SlicePolicy, window int) int {
	cap := pol.ItemCap
	if cap <= 0 {
		cap = pol.OverFetch
	}
	pool := 2 * cap
	if pol.BehaviorFloor > pool {
		pool = pol.BehaviorFloor
	}
	if pool > window {
		pool = window
	}
	if pool < 0 {
		pool = 0
	}
	return pool
}

func containsPassiveRow(rows []passiveRow, id string) bool {
	for _, r := range rows {
		if r.mem.ID == id {
			return true
		}
	}
	return false
}

func passiveTwoPass(rows []passiveRow, pol SlicePolicy, poolCap int) []passiveRow {
	behavioral := make(map[string]bool, len(pol.BehaviorCategories))
	for _, c := range pol.BehaviorCategories {
		behavioral[c] = true
	}
	used := make(map[string]bool, len(rows)+1)
	picked := make([]passiveRow, 0, len(rows))
	counts := make(map[string]int, len(behavioral))
	for {
		best := -1
		var bestScore float64
		for i, r := range rows {
			cat := r.mem.Category
			if used[r.mem.ID] || !behavioral[cat] {
				continue
			}
			if capN, ok := pol.CategoryCaps[cat]; ok && capN > 0 && counts[cat] >= capN {
				continue
			}
			s := r.score
			if w, ok := pol.CategoryWeights[cat]; ok {
				s *= w
			}
			if best == -1 || s > bestScore {
				best, bestScore = i, s
			}
		}
		// The reservation stops at its own budget AND at exhaustion: a window
		// with no behavioral row left is filled by the plain pass below, not by
		// spinning here.
		if best == -1 || len(picked) >= pol.BehaviorFloor {
			break
		}
		used[rows[best].mem.ID] = true
		counts[rows[best].mem.Category]++
		picked = append(picked, rows[best])
	}
	for _, r := range rows {
		if len(picked) >= poolCap {
			break
		}
		if !used[r.mem.ID] {
			used[r.mem.ID] = true
			picked = append(picked, r)
		}
	}
	return picked
}

// passiveDemote applies the two reorders the session-start path has always
// applied, in the order it applied them: supersede first, so a superseded row
// cannot outrank its replacement even when both survive, then near-duplicate.
//
// The near-duplicate step is where the buckets differ. A policy that drops losers
// REMOVES them — the global bucket, whose cap is tight enough that a
// near-duplicate restatement would otherwise spend a slot — and one that does
// not only reorders, leaving the assembler's slice cap to drop the tail.
func (s *Store) passiveDemote(ctx context.Context, rows []passiveRow, pol SlicePolicy) []passiveRow {
	if len(rows) < 2 {
		return rows
	}
	ids := make([]string, len(rows))
	// TWO protection maps, because the two relations protect different things.
	//
	// Supersede protects a row that has been RETAINED: a persistent row is the
	// author's statement that it outlives the session, and a supersede edge into
	// one is a claim being made about a row the tier says is standing. Tier alone.
	//
	// Near-duplicate protects a row from being treated as a RESTATEMENT, and a
	// pinned row is protected for the ordinary reason plus a persistent one. A
	// pin-only map here is the hole DemotionPenalties' own doc names: an unpinned
	// persistent row on the losing end of a pair is penalised here and spared
	// everywhere else — and on a bucket that drops losers, penalised means REMOVED
	// from the session-start block. Every other caller in the tree passes
	// Pinned || RetentionExempt, so this must too or the two orderings disagree
	// about which member of a pair loses.
	supersedeProtected := make(map[string]bool, len(rows))
	nearDupProtected := make(map[string]bool, len(rows))
	for i, r := range rows {
		ids[i] = r.mem.ID
		exempt := RetentionExempt(r.mem)
		if exempt {
			supersedeProtected[r.mem.ID] = true
		}
		nearDupProtected[r.mem.ID] = r.mem.Pinned || exempt
	}
	// A persistent row is exempt from supersede demotion (#709): tier is the
	// author's statement that a memory outlives the session, and a supersede edge
	// into one is a claim being made about a row the tier says is standing. The
	// map is the same one GetTopMemories builds, so the two orderings cannot
	// disagree about which rows a supersede may move.
	if penalty, err := SupersedePenalties(ctx, s.queryDB(), ids, supersedeProtected); err != nil {
		s.logger.Debug("candidates: passive supersede demotion lookup failed", "error", err)
	} else if len(penalty) > 0 {
		rows = StableDemote(rows, func(r passiveRow) string { return r.mem.ID }, penalty)
		// The near-duplicate lookup decides WHICH member of a pair loses from the
		// order it is given, so it has to be the order the supersede demote left
		// behind. Reusing the pre-demote slice ranks a row that has just been
		// pushed down as if it had not moved, and on a bucket that DROPS losers that
		// is a membership decision — the wrong row leaving the block. Both shipped
		// readers rebuild it here for the same reason (hook.go:1219, store.go:3955).
		for i, r := range rows {
			ids[i] = r.mem.ID
		}
	}
	// The over-cap gate. A near-duplicate demotion is a REORDER, so on a selected
	// set that fits entirely under the bucket's cap it can only shuffle rows the
	// answer shows in full — and the shipped loaders both skip it there
	// (loadSessionContext on `len(memories) > sessionMemoriesCap`, GetTopMemories
	// on `len(results) > limit`). Skipping it is the specification; applying it
	// anyway is a different order for the same rows, which a golden comparison
	// against the old loader would read as a regression when the set is small and
	// read as nothing at all when the set is large.
	if pol.DemoteOnlyWhenOverCap && len(rows) <= pol.ItemCap {
		return rows
	}
	// The threshold falls back to the STORE's configured one, which is the same
	// value demoteNearDuplicates uses on the query path. It has to: a policy that
	// states none would otherwise demote at 0.0, and nearDuplicatePenaltyRows
	// binds the threshold as `l.strength >= ?` — so 0.0 makes EVERY `related` edge
	// a near-duplicate, and a global policy that drops losers would then delete a
	// memory over a 0.1-similarity edge. A silent zero here is a row vanishing
	// from a session-start block.
	threshold := pol.DemotionThreshold
	if threshold <= 0 {
		s.mu.RLock()
		threshold = s.demotionThreshold
		s.mu.RUnlock()
	}
	penalty, err := DemotionPenalties(ctx, s.queryDB(), ids, nearDupProtected, threshold)
	if err != nil {
		s.logger.Debug("candidates: passive demotion lookup failed", "error", err)
		return rows
	}
	if len(penalty) == 0 {
		return rows
	}
	if pol.DropDemotedLosers {
		kept := make([]passiveRow, 0, len(rows))
		for _, r := range rows {
			if penalty[r.mem.ID] == 0 {
				kept = append(kept, r)
			}
		}
		return kept
	}
	return StableDemote(rows, func(r passiveRow) string { return r.mem.ID }, penalty)
}

// passiveCandidate materialises one selected row. The rank sentinels are the -1
// "this leg did not retrieve it" values, because no leg ran: a passive block
// reports no leg rank rather than a rank of zero, which is a real first place.
func passiveCandidate(r passiveRow) Candidate {
	c := Candidate{Memory: r.mem}
	c.Base = float64(r.mem.Importance)
	c.AgeDays = r.age
	c.Decay = r.decay
	c.Score = r.score
	c.FTSRank, c.VectorRank, c.VectorScore = -1, -1, -1
	return c
}

// decayRankingSQLAt is DecayRankingSQL with its clock bound, and it returns the
// bindings to go with it.
//
// The clock and the expression are produced TOGETHER, and that is the point
// rather than a convenience: the expression has one placeholder per occurrence of
// julianday('now'), and a caller that substituted one placeholder and bound one
// argument gets a query SQLite refuses with "missing argument" — or, worse, one
// whose clock is bound to the bucket. Deriving both from a single count of the
// occurrences is what keeps them in step, and a test pins the round trip so a
// future edit to the constant cannot silently change the number.
//
// It is derived from the constant rather than written out beside it, because two
// copies of a decay formula are two answers to "how old is this row" and they
// would drift the first time one of them was edited.
func decayRankingSQLAt(now time.Time, hasTier bool) (string, []any) {
	stamp := now.UTC().Format(stampLayoutForSQL)
	base := DecayRankingSQLWithTier(hasTier)
	occurrences := strings.Count(base, "julianday('now')")
	args := make([]any, 0, occurrences)
	for i := 0; i < occurrences; i++ {
		args = append(args, stamp)
	}
	return strings.ReplaceAll(base, "julianday('now')", "julianday(?)"), args
}

// passiveColumns is the shape of the store this request is reading: the SELECT
// list to use, and whether the tier half of the decay order is available.
//
// It exists because a passive read is reached through a NON-MIGRATING handle.
// Every store Ghost opens itself migrates on the way in, so the whole-Memory
// readers have never needed to ask what schema they are looking at — and
// `internal/mcpinit`'s loaders, which read through `OpenReadDB` and so cannot
// migrate, each carry their own probe. A passive retrieval is the first
// whole-Memory read the memory package itself has to make version-tolerant, and
// without this the session-start migration would fail with "no such column:
// retention" on a store below the tier floor, where the loader it replaces
// renders the block perfectly well.
type passiveColumns struct {
	list     string
	HasTier  bool
	HasScope bool
	// HasProvenance gates the EVIDENCE read, which is not a column in the memories
	// table at all: `memory_provenance` is a table migrateV18 creates, so a store
	// below that floor has never had it. Unlike the demotion lookups, which degrade
	// to "no penalty", this one is a returned error — and an error here would turn
	// exactly the pre-tier store the rest of this function exists to support into no
	// session context at all.
	HasProvenance bool
}

// The schema versions that added memories.scope and memories.retention. They are
// the same floors the session-start loaders apply, stated here rather than
// imported: a column cannot be selected on a store that does not have it, and a
// decay order cannot multiply by one that is not there.
const (
	passiveScopeColumnFloor     = 12
	passiveRetentionColumnFloor = 19
	// memory_provenance is a TABLE rather than a column, so it needs its own floor:
	// the SELECT list cannot express "this table may not exist" the way a column
	// can be replaced by a NULL literal.
	passiveProvenanceColumnFloor = 18
)

// passiveColumnsFor resolves the store's shape through the exported version pair,
// so this file does not restate the current schema version.
//
// An UNREADABLE version is treated as the floor rather than as "current". That is
// the safe direction: a store whose version cannot be read is read without the
// columns that might not be there, and the rows that come back carry NULL for
// what they could not have said. Returning an error instead would turn a
// transient PRAGMA failure into no session context at all, which is a worse
// answer than a block missing a tier label.
func passiveColumnsFor(s *Store) (passiveColumns, error) {
	// Both flags start FALSE, which is the safe direction: a column the store may
	// not have is substituted for, so a path that fails to set one loses a label
	// rather than failing the read. The error branch below leaves them false on
	// purpose, and says why.
	var hasScope, hasTier, hasProvenance bool

	// Through the SNAPSHOT, not the pool. This runs inside the read transaction
	// `Candidates` opened, and that pool is pinned at MaxOpenConns(1): the
	// transaction holds the only connection, so a PRAGMA on the pool would wait for
	// a connection that cannot be handed out — a deadlock, not an error.
	version, versionErr := dbUserVersion(s.queryDB())
	if versionErr == nil {
		hasScope = version >= passiveScopeColumnFloor
		hasTier = version >= passiveRetentionColumnFloor
		hasProvenance = version >= passiveProvenanceColumnFloor
	} else {
		s.logger.Debug("candidates: passive read could not read the store's schema version", "error", versionErr)
	}

	// The list is the shared one, with a column this store may not have replaced by
	// a literal of the SHAPE scanMemories expects. Position matters as much as
	// shape: the scanner binds by position, so a shorter list is an argument-count
	// failure rather than a value.
	//
	// `retention` is substituted with an EMPTY STRING, not NULL, and that is not a
	// detail: the scanner binds it as a plain string and resolves `""` to
	// `RetentionProject` itself ("a row whose tier reads empty is a row a query did
	// not select, not a fourth tier"). A NULL here is a Scan error — "converting
	// NULL to string is unsupported" — which is how this was found. `scope` and
	// `expires_at` take NULL because those ARE bound as sql.NullString, and NULL is
	// the honest value for "no scope stated" and "no expiry claimed".
	names := append([]string(nil), memoryColumnNames...)
	for i, c := range names {
		switch {
		case c == "scope" && !hasScope:
			names[i] = "NULL AS scope"
		case c == "retention" && !hasTier:
			names[i] = "'' AS retention"
		case c == "expires_at" && !hasTier:
			names[i] = "NULL AS expires_at"
		}
	}
	return passiveColumns{
		list:          qualifyColumnsFrom(names, ""),
		HasTier:       hasTier,
		HasScope:      hasScope,
		HasProvenance: hasProvenance,
	}, nil
}
