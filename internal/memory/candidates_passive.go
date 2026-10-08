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
	"sort"
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

// readableStampSQL is the SQL boolean for "memory.ParseStamp could read this
// column", and it is a ROUND TRIP rather than a shape check: strftime returns
// NULL for a value no layout parses AND for one whose fields are out of range
// (`2026-02-30`), and the equality rejects a value SQLite would normalize to a
// different string. That is the same set ParseStamp accepts over StampLayouts:
// `2006-01-02 15:04:05` and `2006-01-02`.
//
// A GLOB per layout would be wrong in the direction that loses rows: it accepts
// `9999-99-99`, which Go's parser rejects, and this predicate would then COMPARE
// an unreadable bound where ValidityState treats it as no claim.
//
// The two formats are tried in StampLayouts' order. Go's time.Parse also accepts
// a fractional second after the seconds field of the first layout (`.000`, `.5`,
// `,5`: a separator then one or more digits and nothing else), so the first
// branch tests the leading 19 characters and then the shape of the rest. A row
// with such a bound is read exactly as Go reads it, and is compared through
// stampComparableSQL.
func readableStampSQL(col string) string {
	lead := "substr(" + col + ", 1, 19)"
	seconds := "strftime('%Y-%m-%d %H:%M:%S', " + lead + ")"
	day := "strftime('%Y-%m-%d', " + col + ")"
	fraction := "(length(" + col + ") = 19 OR (length(" + col + ") >= 21" +
		" AND substr(" + col + ", 20, 1) IN ('.', ',')" +
		" AND substr(" + col + ", 21) NOT GLOB '*[^0-9]*'))"
	return "((" + seconds + " IS NOT NULL AND " + lead + " = " + seconds + " AND " + fraction + ")" +
		" OR (" + day + " IS NOT NULL AND " + col + " = " + day + "))"
}

// stampComparableSQL is the column as it is compared against the bound Now, which
// is bound at WHOLE-SECOND precision (stampLayoutForSQL truncates it). A stored
// fraction is cut off so both sides are whole seconds, and the comparison is
// inclusive on both ends. That makes the SQL predicate a SUPERSET of Go's rule:
// truncation can only pull a bound toward the same second as Now, never across
// it, so any row Go keeps (`until >= now`, `from <= now`, at full precision) is
// kept here. It may admit a row Go then drops at stage 2 (from `12:00:00.5` with
// a Now of `12:00:00.3`); that costs one LIMIT slot and nothing else. It never
// drops a row Go calls valid, whatever sub-second part Now carries.
func stampComparableSQL(col string) string {
	return "CASE WHEN length(" + col + ") > 19 THEN substr(" + col + ", 1, 19) ELSE " + col + " END"
}

// validityMatchesSQL is the SQL form of memory.ValidityState's window test: a row
// is admitted when its window contains the bound Now (valid_from <= now AND
// valid_until >= now). It returns the fragment with TWO ? placeholders, bound in
// text order — valid_until then valid_from — both to the same now stamp.
//
// A NULL bound is open, and a bound no layout reads is treated as no claim, which
// is the one rule the two forms must share (see readableStampSQL). A readable
// bound is compared directly, so `valid_until` exactly equal to now is KEPT (the
// rule is a closed window, not an open one), matching ValidityState's
// `until.Before(now)`.
func validityMatchesSQL() string {
	until, from := readableStampSQL("valid_until"), readableStampSQL("valid_from")
	return "(valid_until IS NULL OR NOT " + until + " OR " + stampComparableSQL("valid_until") + " >= ?)" +
		" AND (valid_from IS NULL OR NOT " + from + " OR " + stampComparableSQL("valid_from") + " <= ?)"
}

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
//   - a bucket that admits `_global` AND a policy that fetches `_global` in its
//     own right. The rows overlap, so the set carries every one of them twice.
func validatePassivePolicies(policies []SlicePolicy) error {
	if len(policies) == 0 {
		return ErrPassiveUnsupported
	}
	seen := make(map[string]bool, len(policies))
	mixesGlobal := false
	for _, pol := range policies {
		if pol.Bucket == "" {
			return errors.New("candidates: a passive policy names no bucket, so it would fetch nothing and read as an empty bucket")
		}
		if seen[pol.Bucket] {
			return fmt.Errorf("candidates: two passive policies name bucket %q, so its rows would be fetched twice and every "+
				"one of them returned twice; one policy per bucket", pol.Bucket)
		}
		seen[pol.Bucket] = true
		if pol.IncludeGlobal {
			mixesGlobal = true
		}
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
	// Distinct bucket NAMES, so this is not the repeated-bucket check above. One
	// policy reading `project_id = ? OR project_id = '_global'` and another reading
	// `_global` on its own are two different statements over one overlapping set,
	// and concatenating them returns every global row twice — a duplicate id
	// reaching the assembler, which would then report two rows that happened to
	// rank equally. A caller that wants both the union and a globals-only section
	// runs them as two SEPARATE requests, which is what the project-context
	// surface does.
	if mixesGlobal && seen[GlobalProjectID] {
		return fmt.Errorf("candidates: a policy admits %q into another bucket while a second policy fetches %q on its own; "+
			"the two row sets overlap, so every global row would be returned twice. Read them as two requests, or fetch "+
			"_global under the mixing bucket alone", GlobalProjectID, GlobalProjectID)
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
		fetched, losers, err := s.passiveBucket(ctx, req, pol, cols)
		if err != nil {
			return nil, err
		}
		set.DroppedLosers = append(set.DroppedLosers, losers...)
		// A bucket that came back empty is the only case the reason can be reported
		// on, and the only case the probe has to run: if any bucket returned rows
		// the set is non-empty and the assembler never reads this count. Probing the
		// empty bucket and summing reports the exclusions of a union of two empty
		// buckets, which is what the verdict describes.
		if len(fetched) == 0 {
			excluded, err := s.passiveValidityExcluded(ctx, req, pol, cols)
			if err != nil {
				return nil, err
			}
			set.ValidityExcluded += excluded
		}
		beyond, err := s.passivePinnedBeyond(ctx, req, pol, cols, fetched)
		if err != nil {
			return nil, err
		}
		for project, n := range beyond {
			if set.PinnedBeyond == nil {
				set.PinnedBeyond = map[string]int{}
			}
			set.PinnedBeyond[project] += n
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
	switch cols.evidenceReadMode() {
	case evidenceReadPlain:
		counts, err = evidenceCountsFor(ctx, s.queryDB(), scope)
		if err != nil {
			return nil, fmt.Errorf("candidates: evidence counts: %w", err)
		}
	case evidenceReadAttemptTolerating:
		// The version could not be read, so whether the table exists is UNKNOWN —
		// and skipping the read would report "no recorded evidence" as a fact about
		// support, which is the one claim this must not invent. So it is attempted,
		// and the ONLY failure tolerated is the table being absent. Anything else
		// (a locked file, a corrupt page) is a real error, because a zero in place of
		// it would be a false statement about the corpus rather than a gap in it.
		counts, err = evidenceCountsFor(ctx, s.queryDB(), scope)
		if err != nil {
			if isMissingTable(err) {
				s.logger.Debug("candidates: passive read found no evidence table on a store of unknown version", "error", err)
				counts = map[string]EvidenceCounts{}
				break
			}
			return nil, fmt.Errorf("candidates: evidence counts: %w", err)
		}
	case evidenceReadSkip:
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
func (s *Store) passiveBucket(ctx context.Context, req CandidateRequest, pol SlicePolicy, cols passiveColumns) ([]Candidate, []DroppedLoser, error) {
	query, args := passiveFetchSQL(pol, req, cols)
	rows, err := s.queryDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("candidates: passive fetch for bucket %q: %w", pol.Bucket, err)
	}
	defer func() { _ = rows.Close() }()
	memories, err := scanMemories(rows)
	if err != nil {
		return nil, nil, err
	}
	// The replacement of a pinned row holds a slot too, so it has to be in the
	// window to be given one: a window cut by rank would leave a pinned row that
	// was replaced standing without its replacement.
	replacements, err := s.passiveReplacementsOfPinned(ctx, req, pol, cols, memories)
	if err != nil {
		return nil, nil, err
	}
	memories = append(memories, replacements...)
	return s.selectPassive(ctx, memories, pol, req.Now, pol.Bucket)
}

// passiveReplacementsOfPinned reads the rows that supersede a pinned row of the
// window and are not in it, from the same population and under the same validity
// predicate as the fetch, so a replacement the fetch would withhold is not
// brought in. It costs one statement, and only when the window holds a pinned row.
func (s *Store) passiveReplacementsOfPinned(ctx context.Context, req CandidateRequest, pol SlicePolicy, cols passiveColumns, window []Memory) ([]Memory, error) {
	var pinnedIDs, windowIDs []any
	for _, m := range window {
		windowIDs = append(windowIDs, m.ID)
		if m.Pinned {
			pinnedIDs = append(pinnedIDs, m.ID)
		}
	}
	if len(pinnedIDs) == 0 {
		return nil, nil
	}
	where, args := passivePopulationSQL(pol, req, cols)
	if cols.HasValidity {
		stamp := req.Now.UTC().Format(stampLayoutForSQL)
		where += " AND " + validityMatchesSQL()
		args = append(args, stamp, stamp)
	}
	marks := func(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }
	where += " AND id IN (SELECT source_id FROM memory_links WHERE relation = 'supersedes' AND invalidated_at IS NULL AND target_id IN (" +
		marks(len(pinnedIDs)) + ")) AND id NOT IN (" + marks(len(windowIDs)) + ")"
	args = append(args, pinnedIDs...)
	args = append(args, windowIDs...)
	rows, err := s.queryDB().QueryContext(ctx, "SELECT "+cols.list+" FROM memories WHERE "+where, args...)
	if err != nil {
		return nil, fmt.Errorf("candidates: passive replacements of pinned rows for bucket %q: %w", pol.Bucket, err)
	}
	defer func() { _ = rows.Close() }()
	return scanMemories(rows)
}

// passivePopulationSQL is the WHERE half a bucket's fetch and the validity probe
// that counts what the filter removed from it SHARE: the policy's project
// predicate with its bucket binding, `resolved_at IS NULL`, and the scope
// predicate where the store has the column. It is one function so the two
// statements cannot disagree about which rows the window is over — a probe run
// over a different population would report exclusions the fetch never
// considered, and the difference is exactly the row set the verdict describes.
//
// The project predicate is the POLICY'S BUCKET, and `CandidateRequest.Mode` does
// not apply here. That is the whole shape of a passive read: one statement per
// bucket, each naming its own project, where the query path folds project and
// `_global` into a single leg with a mode selecting the predicate. A caller that
// wants both states two policies, which is what the assembler's budget slices
// become — and which is why nothing here merges a project row and a global row in
// one result set for the assembler to have to separate.
//
// PARENTHESISED on the global half, and that is not decoration. `AND` binds
// tighter than `OR` in SQL, so an unbracketed `project_id = ? OR project_id =
// '_global' AND resolved_at IS NULL` reads as `project_id = ? OR (… AND
// resolved_at IS NULL)` — every row of the requesting project escapes the
// resolved filter, and the scope clause is scoped to the `_global` half alone.
// The block then renders resolved rows, and a scope filter that was set does
// nothing to the rows that matter. The goldens' resolved-row guard is what caught
// it.
//
// The scope predicate contributes NO bindings: ScopeMatchesSQL embeds the
// requested values as a quoted JSON literal, so a value holding a quote is escaped
// rather than allowed to end the statement early. Its own guard is the
// session-start loaders': on a store below the scope floor the column is not
// there, and every row carries no scope, so an unscoped row never conflicts with a
// session scope — the substitution shows the rows that store has, which is the
// block it produced before scope was read.
func passivePopulationSQL(pol SlicePolicy, req CandidateRequest, cols passiveColumns) (string, []any) {
	projectClause := "project_id = ?"
	if pol.IncludeGlobal {
		projectClause = "(" + projectClause + " OR project_id = '" + GlobalProjectID + "')"
	}
	where := projectClause + " AND resolved_at IS NULL"
	if cols.HasScope && len(req.Scope) > 0 {
		where += " AND " + ScopeMatchesSQL("scope", req.Scope)
	}
	return where, []any{pol.Bucket}
}

// passiveFetchSQL builds one policy's read and its bindings TOGETHER, because a
// mismatch between an ORDER BY and its argument list is not an error — it binds
// the clock to the wrong column, and a fully decayed row then reads as a fresh
// one with no complaint anywhere.
//
// The validity predicate is applied HERE, in SQL, rather than only in the
// assembler's stage 2. The over-fetch chooses which rows are read at all, so a row
// whose validity window has closed or has not opened must not spend any of the
// window: filtering it afterwards would fill the window with rows the caller
// cannot use and then cut them, and a window that small reaches a weaker block.
// It is GATED on the column existing, because on a store below the validity floor
// the predicate names columns that are not there and the whole fetch would fail
// with "no such column: valid_from". The rows the block then shows are the ones
// that store has, labelled without a validity window — the block it produced
// before validity was read.
//
// The bindings are in TEXT order: the bucket, then the validity stamps when the
// predicate is present, then the clock for the decay order, then the limit. The
// validity stamps are bound rather than interpolated, so the same clock value the
// decay ranking and the selection use is the one the window is filtered against.
func passiveFetchSQL(pol SlicePolicy, req CandidateRequest, cols passiveColumns) (string, []any) {
	where, args := passivePopulationSQL(pol, req, cols)
	if cols.HasValidity {
		// The clock is bound to the same instant the decay ranking uses, so the
		// window, the decay score derived from it in selectPassive, and the row
		// ages the trace reports are all made against the same clock.
		stamp := req.Now.UTC().Format(stampLayoutForSQL)
		where += " AND " + validityMatchesSQL()
		args = append(args, stamp, stamp)
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
	if pol.Order == "" || pol.Order == OrderDecay {
		// The rank expression takes a BOUND instant rather than julianday('now'),
		// so the window, the decay score derived from it in selectPassive, and
		// the row ages the trace reports are all made against the same clock. The
		// wall-clock form is what the shipped loaders interpolate, and using it
		// here would reintroduce the drift a bound Now exists to remove, at a
		// moment when the two reads can straddle a second boundary.
		rank, clock := decayRankingSQLAt(req.Now, cols.HasTier)
		// `pinned DESC` leads, and it is the half of the pin's slot guarantee that
		// decides whether a pinned row is in the window AT ALL: the LIMIT cuts the
		// window by this order, and a pinned row at importance 0.1 ranks behind
		// every 0.9 row the bucket holds, so a window of N rows full of better ones
		// never contains it. The decay score keeps ordering the pinned rows among
		// themselves (a pin exempts a row from decay, not from importance), and
		// ordering the rest exactly as it did.
		orderBy = fmt.Sprintf("pinned DESC, (%s) DESC, importance DESC, created_at DESC, id", rank)
		args = append(args, clock...)
	}

	// The column list is the shared one every whole-Memory reader selects, rather
	// than a hand-written copy: the retention change added two columns and a copy
	// here would have failed at Scan with a count mismatch, which is a loud failure
	// but still a second statement of what a Memory is. `memoryColumns` is derived
	// from `memoryColumnNames`, which is the single list.
	//
	// The predicate is a FRAGMENT spliced in, not a bind, because it is built from
	// a policy field and the constant `_global` — never from caller text. The one
	// caller-supplied value, the bucket, is bound in passivePopulationSQL.
	query := fmt.Sprintf(`
		SELECT %s
		FROM memories
		WHERE %s
		ORDER BY %s
		LIMIT ?`, cols.list, where, orderBy)
	return query, append(args, pol.OverFetch)
}

// passivePinnedBeyond counts the pinned rows the window's LIMIT left unread, by
// the row's own project (a union bucket reads `_global` rows too, and the cut
// belongs to the project the rows are shown under). The fetch orders pinned rows
// first, so a pinned row can only be past the LIMIT when the whole window is
// pinned; any other window already holds every pinned row the population has, and
// the count is not run. It asks the same population and the same validity
// predicate the fetch does, so a row the fetch withholds is not counted as one the
// window missed.
func (s *Store) passivePinnedBeyond(ctx context.Context, req CandidateRequest, pol SlicePolicy, cols passiveColumns, fetched []Candidate) (map[string]int, error) {
	if len(fetched) == 0 || len(fetched) < pol.OverFetch {
		return nil, nil
	}
	read := map[string]int{}
	for _, c := range fetched {
		if !c.Pinned {
			return nil, nil
		}
		read[c.ProjectID]++
	}
	where, args := passivePopulationSQL(pol, req, cols)
	if cols.HasValidity {
		stamp := req.Now.UTC().Format(stampLayoutForSQL)
		where += " AND " + validityMatchesSQL()
		args = append(args, stamp, stamp)
	}
	rows, err := s.queryDB().QueryContext(ctx, "SELECT project_id, COUNT(*) FROM memories WHERE "+where+" AND pinned = 1 GROUP BY project_id", args...)
	if err != nil {
		return nil, fmt.Errorf("candidates: passive pinned count for bucket %q: %w", pol.Bucket, err)
	}
	defer func() { _ = rows.Close() }()
	beyond := map[string]int{}
	for rows.Next() {
		var project string
		var n int
		if err := rows.Scan(&project, &n); err != nil {
			return nil, fmt.Errorf("candidates: passive pinned count for bucket %q: %w", pol.Bucket, err)
		}
		if n > read[project] {
			beyond[project] = n - read[project]
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("candidates: passive pinned count for bucket %q: %w", pol.Bucket, err)
	}
	return beyond, nil
}

// PassiveEligibleCount is how many rows the policy's bucket holds that the
// window's fetch could ever draw from, and how many of those the fetch's own
// validity predicate removes before the LIMIT. Both come from ONE statement over
// passivePopulationSQL, the population the fetch and the validity probe already
// share, so the count and the window cannot disagree about which rows are in
// play and a validity-excluded row is counted once: inside `eligible`, and again
// as `validityExcluded`, never as a second row. `eligible - validityExcluded` is
// the number of rows the window could hold if it had no LIMIT.
//
// A session-start header uses it to say how many rows the store holds against the
// over-fetch's window, so a bucket larger than the window does not read as if it
// held only the window, and rows the predicate withheld are reported as withheld
// rather than ranked out. A store below the validity floor has no predicate:
// validityExcluded is zero.
func (s *Store) PassiveEligibleCount(ctx context.Context, pol SlicePolicy, now time.Time, scope map[string]string) (eligible, validityExcluded int, err error) {
	cols, err := passiveColumnsFor(s)
	if err != nil {
		return 0, 0, err
	}
	where, popArgs := passivePopulationSQL(pol, CandidateRequest{Scope: scope, Now: now}, cols)
	if !cols.HasValidity {
		err = s.queryDB().QueryRowContext(ctx, "SELECT COUNT(*) FROM memories WHERE "+where, popArgs...).Scan(&eligible)
		if err != nil {
			return 0, 0, fmt.Errorf("candidates: passive eligible count for bucket %q: %w", pol.Bucket, err)
		}
		return eligible, 0, nil
	}
	stamp := now.UTC().Format(stampLayoutForSQL)
	// The select list's placeholders come first in the statement's text, so the
	// stamps are bound before the population's bucket.
	args := append([]any{stamp, stamp}, popArgs...)
	query := "SELECT COUNT(*), COALESCE(SUM(CASE WHEN " + validityMatchesSQL() + " THEN 0 ELSE 1 END), 0) FROM memories WHERE " + where
	if err := s.queryDB().QueryRowContext(ctx, query, args...).Scan(&eligible, &validityExcluded); err != nil {
		return 0, 0, fmt.Errorf("candidates: passive eligible count for bucket %q: %w", pol.Bucket, err)
	}
	return eligible, validityExcluded, nil
}

// passiveValidityExcluded counts the rows in one bucket's window that the validity
// predicate removed BEFORE the LIMIT — the rows that would have been fetched had
// they been in their window. It exists because filtering in SQL leaves the
// assembler a set that is empty with no stage having run, and "the window came
// back empty" (no_memories) and "rows were found and withheld as out of date"
// (all_invalid) are different facts a caller acts on differently; the census
// surface may claim absence only for the first.
//
// The probe is run only for a bucket whose fetch came back empty, so a session
// start that reads anything pays nothing for it. It asks the same population the
// fetch does, through passivePopulationSQL, so a row excluded by scope or by
// resolved_at is not counted as a validity exclusion.
//
// Every counted row is one Go also calls invalid, because the predicate is a
// superset of Go's verdict (see stampComparableSQL); the count is a LOWER BOUND on
// what stage 2 would have withheld (a row admitted only by second-truncation is
// not counted), never an overcount.
//
// A store below the validity floor has no predicate to have excluded anything:
// zero, not an error.
func (s *Store) passiveValidityExcluded(ctx context.Context, req CandidateRequest, pol SlicePolicy, cols passiveColumns) (int, error) {
	if !cols.HasValidity {
		return 0, nil
	}
	where, args := passivePopulationSQL(pol, req, cols)
	stamp := req.Now.UTC().Format(stampLayoutForSQL)
	// NOT (validityMatchesSQL()) is "the predicate would have dropped this row".
	// The two ? placeholders are bound in the predicate's own text order (until,
	// then from), after the population's bucket.
	args = append(args, stamp, stamp)
	query := `SELECT COUNT(*) FROM memories WHERE ` + where + ` AND NOT (` + validityMatchesSQL() + `)`
	var n int
	if err := s.queryDB().QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("candidates: passive validity exclusion count for bucket %q: %w", pol.Bucket, err)
	}
	return n, nil
}

// selectPassive applies the policy's selection and demotions, and returns the
// selected rows followed by the rest of the window, and separately the
// near-duplicate losers a DropDemotedLosers policy removed from it.
func (s *Store) selectPassive(ctx context.Context, memories []Memory, pol SlicePolicy, now time.Time, fetchedBy string) ([]Candidate, []DroppedLoser, error) {
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
	//
	// Validity is judged BEFORE the selection and the demotions. A row outside its
	// window is withheld by stage 2 whatever happens here, so it must not fill a
	// pool slot, outrank a live row, or be the winner of an edge that demotes or
	// (on a bucket that drops losers) removes one: a row stage 2 is about to drop
	// would cost a live row its place and leave nothing in exchange (#893). The
	// fetch states the same window in SQL, so this is normally a no-op; it is the
	// order that is the invariant, and it holds for rows that reach this function
	// by any other route. Withheld rows are still returned, behind everything
	// eligible, because stage 2 is the authority on dropping them and reports it.
	eligible, withheld := passiveEligible(scored, now)
	// The pin's slot guarantee, ahead of ranking. The selection runs over every
	// eligible row exactly as before, so a pinned row still counts toward the
	// behavioural floor it belongs to. The reserved rows are then brought in from
	// the tail and lead the set the demotions run over, so they are never outside
	// the selection and never the loser of a near-duplicate pair.
	//
	// A pin is a slot, not a rank above the row that replaced it: a pinned row with
	// a live `supersedes` edge keeps its slot but is ordered directly behind its
	// superseder, and the superseder is reserved too, because a cap that cut it
	// would leave the pinned row standing alone on a page that says it was replaced.
	supersededBy := s.passivePinnedSupersededBy(ctx, eligible)
	chosen, rest := passiveSelect(eligible, pol)
	chosen, rest = passiveReserve(chosen, rest, supersededBy)
	chosen, removed := s.passiveDemote(ctx, chosen, pol)
	// A demotion is a reorder and sinks a superseded row; the final arrangement
	// puts the reserved rows back at the head, in the order the demotion left them,
	// each superseded pin directly behind its superseder.
	chosen = passiveArrange(chosen, supersededBy)
	rest = append(rest, withheld...)

	out := make([]Candidate, 0, len(scored))
	for _, r := range append(chosen, rest...) {
		c := passiveCandidate(r, fetchedBy)
		c.SupersededBy = supersededBy[r.mem.ID]
		out = append(out, c)
	}
	var losers []DroppedLoser
	for _, l := range removed {
		losers = append(losers, DroppedLoser{Candidate: passiveCandidate(l.row, fetchedBy), LostTo: l.lostTo})
	}
	return out, losers, nil
}

// passivePinnedSupersededBy reads, for each PINNED eligible row, the ids of the
// eligible rows that supersede it. The edge rules are the demotion's own
// (supersedePenaltyRows: the scope exemption, and a persistent-retention target
// is never sunk), so a row is named here exactly when the demotion would sink it.
// A failed read is logged and means no row is named, which leaves the pins first
// and unmarked — the order before the marker existed.
func (s *Store) passivePinnedSupersededBy(ctx context.Context, rows []passiveRow) map[string][]string {
	ids := make([]string, len(rows))
	protected := make(map[string]bool, len(rows))
	pinned := make(map[string]bool, len(rows))
	for i, r := range rows {
		ids[i] = r.mem.ID
		if RetentionExempt(r.mem) {
			protected[r.mem.ID] = true
		}
		if r.mem.Pinned {
			pinned[r.mem.ID] = true
		}
	}
	against, err := supersedePenaltyRows(ctx, s.queryDB(), ids, protected)
	if err != nil {
		s.logger.Warn("candidates: passive supersede lookup for pinned rows failed", "error", err)
		return nil
	}
	out := map[string][]string{}
	for target, sources := range against {
		if !pinned[target] {
			continue
		}
		sorted := append([]string(nil), sources...)
		sort.Strings(sorted)
		out[target] = sorted
	}
	return out
}

// passiveReserved reports whether a row holds a reserved slot: a pinned row, or
// the replacement of one.
func passiveReserved(r passiveRow, replacements map[string]bool) bool {
	return r.mem.Pinned || replacements[r.mem.ID]
}

func passiveReplacements(supersededBy map[string][]string) map[string]bool {
	out := map[string]bool{}
	for _, srcs := range supersededBy {
		for _, id := range srcs {
			out[id] = true
		}
	}
	return out
}

// passiveReserve moves every reserved row the selection left in the tail into
// the selected set and arranges the reserved rows at its head.
func passiveReserve(chosen, rest []passiveRow, supersededBy map[string][]string) (newChosen, newRest []passiveRow) {
	repl := passiveReplacements(supersededBy)
	var tailReserved []passiveRow
	for _, r := range rest {
		if passiveReserved(r, repl) {
			tailReserved = append(tailReserved, r)
		} else {
			newRest = append(newRest, r)
		}
	}
	return passiveArrange(append(chosen, tailReserved...), supersededBy), newRest
}

// passiveArrange is a stable partition with one exception. The reserved rows
// (pinned rows and the replacements of pinned rows) come first, each group in the
// order it arrived — except a superseded pinned row, which is taken out of the
// reserved run and placed directly behind its first-ranked superseder, so a pin
// never outranks the row that replaced it. The unpinned, unreplaced rows follow.
func passiveArrange(rows []passiveRow, supersededBy map[string][]string) []passiveRow {
	repl := passiveReplacements(supersededBy)
	var head, tail []passiveRow
	var behind []passiveRow
	for _, r := range rows {
		switch {
		case len(supersededBy[r.mem.ID]) > 0:
			behind = append(behind, r)
		case passiveReserved(r, repl):
			head = append(head, r)
		default:
			tail = append(tail, r)
		}
	}
	// Each superseded pin goes behind its first-ranked superseder in the head; one
	// whose superseders are all themselves superseded pins (a chain) goes behind
	// the last placed of them, so iterate until nothing more can be placed.
	for len(behind) > 0 {
		var still []passiveRow
		placed := false
		for _, r := range behind {
			at := -1
			for i, h := range head {
				if containsString(supersededBy[r.mem.ID], h.mem.ID) {
					at = i
					break
				}
			}
			if at < 0 {
				still = append(still, r)
				continue
			}
			// Behind any rows already sitting directly behind that superseder.
			at++
			for at < len(head) && len(supersededBy[head[at].mem.ID]) > 0 && containsString(supersededBy[head[at].mem.ID], head[at-1].mem.ID) {
				at++
			}
			head = append(head[:at], append([]passiveRow{r}, head[at:]...)...)
			placed = true
		}
		behind = still
		if !placed {
			// A superseder that is not in the arranged set (it was cut by validity or
			// never fetched): the row keeps its slot at the end of the reserved run.
			head = append(head, behind...)
			break
		}
	}
	return append(head, tail...)
}

func containsString(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// passiveEligible splits rows into those inside their validity window at now and
// those outside it, each in its input order. The rule is ValidityState's, the
// same call stage 2 makes, so a row withheld here is a row stage 2 would drop:
// only an expired or not-yet-valid state is withheld, and an unreadable bound is
// no claim.
func passiveEligible(rows []passiveRow, now time.Time) (eligible, withheld []passiveRow) {
	eligible = make([]passiveRow, 0, len(rows))
	for _, r := range rows {
		switch state, _ := ValidityState(r.mem.ValidFrom, r.mem.ValidUntil, r.mem.VerifiedAt, now); state {
		case ValidityExpired, ValidityFuture:
			withheld = append(withheld, r)
		default:
			eligible = append(eligible, r)
		}
	}
	return eligible, withheld
}

// passiveLoser is a row passiveDemote removed, with the ids it lost to.
type passiveLoser struct {
	row    passiveRow
	lostTo []string
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
//
// The second result is the rows a DropDemotedLosers policy removed, each with the
// winners it lost to, read from the same edge verdicts the removal was decided on.
func (s *Store) passiveDemote(ctx context.Context, rows []passiveRow, pol SlicePolicy) ([]passiveRow, []passiveLoser) {
	if len(rows) < 2 {
		return rows, nil
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
		// Warn, and the reason is the consequence: a store this lookup cannot read
		// re-offers a superseded preference as a live claim, and no surface that
		// renders the result can tell that from a store with nothing superseded.
		// The session-start loaders this replaced printed the same three failures
		// on stderr, and a caller that renders the block has no other way to learn
		// its rows were not demoted.
		s.logger.Warn("candidates: passive supersede demotion lookup failed", "error", err)
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
		return rows, nil
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
	// The verdicts, not just the penalty: a dropped loser has to name the row it
	// lost to, and it is read from the one edge pass that decided the loss.
	pairs, err := nearDuplicatePenaltyRows(ctx, s.queryDB(), ids, nearDupProtected, threshold)
	penalty, against := demotionVerdicts(pairs)
	if err != nil {
		// Warn for the reason the supersede lookup above gives: the near-duplicate
		// pass decides which member of a pair loses, so a store it cannot read
		// shows both, as two independent claims.
		s.logger.Warn("candidates: passive demotion lookup failed", "error", err)
		return rows, nil
	}
	if len(penalty) == 0 {
		return rows, nil
	}
	if pol.DropDemotedLosers {
		kept := make([]passiveRow, 0, len(rows))
		var removed []passiveLoser
		for _, r := range rows {
			if penalty[r.mem.ID] == 0 {
				kept = append(kept, r)
				continue
			}
			removed = append(removed, passiveLoser{row: r, lostTo: against[r.mem.ID]})
		}
		return kept, removed
	}
	return StableDemote(rows, func(r passiveRow) string { return r.mem.ID }, penalty), nil
}

// passiveCandidate materialises one selected row. The rank sentinels are the -1
// "this leg did not retrieve it" values, because no leg ran: a passive block
// reports no leg rank rather than a rank of zero, which is a real first place.
func passiveCandidate(r passiveRow, fetchedBy string) Candidate {
	c := Candidate{Memory: r.mem}
	c.Base = float64(r.mem.Importance)
	c.AgeDays = r.age
	c.Decay = r.decay
	c.Score = r.score
	c.FTSRank, c.VectorRank, c.VectorScore = -1, -1, -1
	// The POLICY, not the row's own project. A bucket that admits `_global` reads
	// rows belonging to two projects under one name, and the caller's cap belongs
	// to the name — see Candidate.FetchedBy for why re-deriving it from the row
	// would leave those rows unbounded.
	c.FetchedBy = fetchedBy
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
	list        string
	HasTier     bool
	HasScope    bool
	HasValidity bool
	// HasProvenance gates the EVIDENCE read, which is not a column in the memories
	// table at all: `memory_provenance` is a table migrateV18 creates, so a store
	// below that floor has never had it. Unlike the demotion lookups, which degrade
	// to "no penalty", this one is a returned error — and an error here would turn
	// exactly the pre-tier store the rest of this function exists to support into no
	// session context at all.
	//
	// It is TRUE for a store KNOWN to have the table and FALSE only for one known
	// to be below the floor. An unreadable version leaves it UNSET, which is the
	// third state and the one that matters: substituting for a column that might
	// exist is the safe direction, but SKIPPING a read that might have succeeded is
	// not, because a skipped read reports "no recorded evidence" — a claim about
	// support that the store may well be able to contradict. The two directions are
	// opposite, and the flag only has one of them.
	HasProvenance bool
	// ProvenanceKnown reports whether the version was READ. The evidence read uses
	// it to choose between three behaviours: read it (known and present), report
	// zero (known and absent), or read it and let a missing table be the only
	// tolerated failure (unknown).
	ProvenanceKnown bool
}

// The schema versions at which the columns this reader depends on arrived on
// memories: scope at v12, the validity triple (valid_from, valid_until,
// verified_at) at v10 through phase1aProvenanceColumns, retention and expires_at
// at v19. Each is stated here rather than imported because a column cannot be
// selected on a store that does not have it, and a decay order cannot multiply by
// one that is not there. The floor is the version the migration STAMPS, not a
// later version that happened to rebuild the table with the same columns:
// migrateV10 introduced them and rebuildMemoriesV15 only carried them across, so
// a v12 store is read with the validity predicate exactly as a current one is.
const (
	passiveScopeColumnFloor     = 12
	passiveRetentionColumnFloor = 19
	passiveValidityColumnFloor  = 10
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
	var hasScope, hasTier, hasValidity, hasProvenance bool
	var versionKnown bool

	// Through the SNAPSHOT, not the pool. This runs inside the read transaction
	// `Candidates` opened, and that pool is pinned at MaxOpenConns(1): the
	// transaction holds the only connection, so a PRAGMA on the pool would wait for
	// a connection that cannot be handed out — a deadlock, not an error.
	version, versionErr := dbUserVersion(s.queryDB())
	if versionErr == nil {
		versionKnown = true
		hasScope = version >= passiveScopeColumnFloor
		hasTier = version >= passiveRetentionColumnFloor
		hasProvenance = version >= passiveProvenanceColumnFloor
		hasValidity = version >= passiveValidityColumnFloor
	} else {
		// Warn, and the reason is what the substitutions below are FOR: a version
		// this cannot read means every flag stays false, so the fetch silently
		// drops its scope filter, its tier label and its expiry window, its validity window and the
		// block renders as an unscoped, undated one. The session-start loaders
		// this replaced printed exactly this diagnosis on stderr and said why it
		// had to be loud: the fallbacks keep working, which is what makes the loss
		// invisible from the outside.
		s.logger.Warn("candidates: passive read could not read the store's schema version", "error", versionErr)
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
	// NULL to string is unsupported" — which is how this was found. `scope`,
	// `expires_at` and the validity triple take NULL because those ARE bound as
	// sql.NullString, and NULL is the honest value for "no scope stated", "no
	// expiry claimed" and "no validity window stated".
	//
	// The validity substitution is what makes the validity floor honest: gating
	// only the WHERE predicate leaves the SELECT list naming `valid_from`, so a
	// store below v10 fails the read with `no such column: valid_from` before the
	// predicate is ever reached. A store that predates validity has no window for
	// any row, which is exactly `NULL`. This is a deliberate second mention of the
	// triple (the first is the predicate in passiveFetchSQL), and
	// TestEveryVerifiedAtMentionIsClassified classifies it as the reader it is.
	names := append([]string(nil), memoryColumnNames...)
	for i, c := range names {
		switch {
		case c == "scope" && !hasScope:
			names[i] = "NULL AS scope"
		case c == "retention" && !hasTier:
			names[i] = "'' AS retention"
		case c == "expires_at" && !hasTier:
			names[i] = "NULL AS expires_at"
		case c == "valid_from" && !hasValidity:
			names[i] = "NULL AS valid_from"
		case c == "valid_until" && !hasValidity:
			names[i] = "NULL AS valid_until"
		case c == "verified_at" && !hasValidity:
			names[i] = "NULL AS verified_at"
		}
	}
	return passiveColumns{
		list:            qualifyColumnsFrom(names, ""),
		HasTier:         hasTier,
		HasScope:        hasScope,
		HasValidity:     hasValidity,
		HasProvenance:   hasProvenance,
		ProvenanceKnown: versionKnown,
	}, nil
}

// isMissingTable reports whether an error is the absence of a TABLE, which is the
// one evidence-read failure that means "this store has never recorded an
// observation" rather than "this read did not work".
//
// Matched on the message rather than on a driver type, because both drivers Ghost
// has used spell it the same way and the alternative — a per-driver probe — would
// be a second thing to keep correct. The risk of over-matching is bounded: a query
// that read no table and failed for another reason still returns an error rather
// than a zero, because only "no such table" carries this wording.
func isMissingTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table")
}

// evidenceReadMode is what a passive read does about the evidence table, and it is
// a named function returning a closed set of three rather than a condition inline
// at the call site, for one reason: the middle case — a version that could not be
// READ — is not reachable from a test by any means short of breaking the database
// handle, and an unreachable branch in a path that decides whether to claim a
// memory is supported is exactly the branch that needs pinning.
type evidenceReadMode int

const (
	// evidenceReadPlain reads the table and treats any failure as an error.
	evidenceReadPlain evidenceReadMode = iota
	// evidenceReadAttemptTolerating reads the table and treats ONLY a missing
	// table as zero, because the store's version is unknown and a skipped read
	// would be a claim it might contradict.
	evidenceReadAttemptTolerating
	// evidenceReadSkip reports zero without reading, which is honest only when the
	// store is KNOWN to predate the table.
	evidenceReadSkip
)

// evidenceReadMode is the three-way decision, and the order matters: what the store
// is KNOWN to have is asked first, because "present" is the only state that admits
// the plain read, and "unknown" is checked before "absent" so an unreadable version
// never takes the skipping branch.
func (c passiveColumns) evidenceReadMode() evidenceReadMode {
	switch {
	case c.HasProvenance:
		return evidenceReadPlain
	case !c.ProvenanceKnown:
		return evidenceReadAttemptTolerating
	default:
		return evidenceReadSkip
	}
}
