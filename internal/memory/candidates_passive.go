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
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
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
func validityMatchesSQL() string { return validityMatchesSQLFor("") }

// validityMatchesSQLFor is validityMatchesSQL with every column qualified by
// prefix, for a statement that cannot name them unqualified: the query path's
// keyword leg aliases memories as m and joins memories_fts as f, so an
// unqualified `valid_until` is at best a name resolved by convention and at
// worst ambiguous. The passive path, a single `FROM memories`, passes "" and
// gets the bare form. It is the same fragment either way, so the two paths
// cannot disagree about what the window test is.
func validityMatchesSQLFor(prefix string) string {
	until, from := readableStampSQL(prefix+"valid_until"), readableStampSQL(prefix+"valid_from")
	return "(" + prefix + "valid_until IS NULL OR NOT " + until + " OR " + stampComparableSQL(prefix+"valid_until") + " >= ?)" +
		" AND (" + prefix + "valid_from IS NULL OR NOT " + from + " OR " + stampComparableSQL(prefix+"valid_from") + " <= ?)"
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
	// Each bucket's rows are one contiguous run of the set, in the bucket's own
	// order; the replacement arrangement below works inside a run, never across
	// two, because each run is cut by its own cap.
	var runs []int
	for _, pol := range req.Passive {
		fetched, losers, err := s.passiveBucket(ctx, req, pol, cols)
		if err != nil {
			return nil, err
		}
		// Rows the set carries past this bucket's over-fetch are the pinned rows'
		// replacements; counted so a header does not count them as "beyond" too.
		if extra := len(fetched) + len(losers) - pol.OverFetch; extra > 0 {
			if set.WindowExtra == nil {
				set.WindowExtra = map[string]int{}
			}
			set.WindowExtra[pol.Bucket] = extra
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
		runs = append(runs, len(fetched))
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

	// The replacements no lifecycle has related yet (lexicalReplacements), read
	// against the edges just loaded so an explicit relation wins. They are NAMED
	// here and ordered by the assembler (arrangeReplacements, just before stage
	// 9), the one place that knows the cap and which rows a stage reserved. A failed edge
	// read leaves the set as it was, unordered and unmarked: the guess must not be
	// made when the edge that would overrule it cannot be seen.
	if status.Status != edgesErr {
		at := 0
		for _, n := range runs {
			run := rows[at : at+n]
			if repl := lexicalReplacements(run, edges, req.Now); len(repl) > 0 {
				for i := range run {
					run[i].MaybeReplacedBy = repl[run[i].ID]
				}
			}
			at += n
		}
	}

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
	memories, err := s.passiveWindow(ctx, req, pol, cols)
	if err != nil {
		return nil, nil, fmt.Errorf("candidates: passive fetch for bucket %q: %w", pol.Bucket, err)
	}
	// The replacement of a pinned row holds a slot too, so it has to be in the
	// window to be given one: a window cut by rank would leave a pinned row that
	// was replaced standing without its replacement.
	replacements, err := s.passiveReplacementsOfPinned(ctx, req, pol, cols, memories)
	if err != nil {
		// Fail open, as the two demotion lookups on this path do for the same table:
		// a store whose links cannot be read loses the replacement reservation and
		// the marker, not every row of the block.
		s.logger.Warn("candidates: passive replacement lookup for pinned rows failed", "error", err)
		replacements = nil
	}
	memories = append(memories, replacements...)
	return s.selectPassive(ctx, memories, pol, req.Now, pol.Bucket)
}

// passiveKeyScanVisited counts the rows the threshold scan has read since the
// last reset, and it exists so a test can pin the READ COUNT rather than a
// duration: the property the scan is for is that a bucket's low-ranked tail is
// never read, and that is a number, not a timing. It is process state rather
// than a field because the scan is a free function over a cursor and no caller
// should have to carry a counter it does not read.
var passiveKeyScanVisited atomic.Int64

// resetPassiveKeyScan resets the read count, for the tests that pin it.
func resetPassiveKeyScan() { passiveKeyScanVisited.Store(0) }

// passiveKeyScanReads is the read count, for the tests that pin it.
func passiveKeyScanReads() int64 { return passiveKeyScanVisited.Load() }

// passiveWindow is a policy's window: the top OverFetch rows under the policy's
// own order, kept by a THRESHOLD SCAN rather than by sorting the bucket.
//
// The scan is exact because of one fact about the rank: every factor of the
// decay — the category factor, the tier factor and the pin's — is at most 1, so
// a row's rank is never above its importance (see decayRankingSQL and
// RetentionDecayFactor). Rows are therefore read in `importance DESC` order,
// which `idx_memories_project_imp` serves, and no row still unread can outrank
// the window once the next row's importance falls below the window's k-th rank:
// an unread row's rank is at most its importance.
//
// Pinned rows are read as their own population and seed the window, because a
// pin outranks every unpinned row whatever its rank. Once the window is full of
// pinned rows nothing unpinned can enter it, so the second scan is skipped.
//
// The window holds KEYS, not rows: the scan reads only the columns the rank
// needs, and the full rows are hydrated once, for the window's k rows, so the
// bucket's content is never read at all.
func (s *Store) passiveWindow(ctx context.Context, req CandidateRequest, pol SlicePolicy, cols passiveColumns) ([]Memory, error) {
	pinned, err := s.scanPassiveKeys(ctx, req, pol, cols, true, nil)
	if err != nil {
		return nil, err
	}
	window := pinned
	if len(window) < pol.OverFetch {
		window, err = s.scanPassiveKeys(ctx, req, pol, cols, false, window)
		if err != nil {
			return nil, err
		}
	}
	return s.hydratePassiveWindow(ctx, window, cols)
}

// scanPassiveKeys is one threshold scan: the pinned population or the unpinned
// one, read in importance order and offered to the window until the stop
// condition fires. `window` is the best-first window to seed, so the pinned scan
// seeds the unpinned one.
func (s *Store) scanPassiveKeys(ctx context.Context, req CandidateRequest, pol SlicePolicy, cols passiveColumns, pinned bool, window []passiveRankKey) ([]passiveRankKey, error) {
	query, args := passiveKeyScanSQL(pol, req, cols, pinned)
	rows, err := s.queryDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("candidates: passive key scan for bucket %q: %w", pol.Bucket, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		key, err := scanPassiveRankKey(rows, req.Now)
		if err != nil {
			return nil, err
		}
		passiveKeyScanVisited.Add(1)
		if len(window) >= pol.OverFetch && passiveRankKeyStopsAt(key, window[len(window)-1], pol.Order) {
			// key and every row after it rank below the window's last place, so
			// none of them can enter it. Stopping here is what makes the read
			// O(window) instead of O(bucket).
			break
		}
		window = insertPassiveRankKey(window, key, pol.Order, pol.OverFetch)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return window, nil
}

// scanPassiveRankKey scans one key row and computes its rank. Importance is
// scanned as the double the column holds rather than through Memory.Importance
// (a float32), because the tie it breaks must be the tie the SQL ordered on: a
// value the store holds that float32 cannot name would otherwise rank differently
// here than it did in the engine.
func scanPassiveRankKey(rows *sql.Rows, now time.Time) (passiveRankKey, error) {
	var key passiveRankKey
	var pinned int
	if err := rows.Scan(&key.id, &key.importance, &pinned, &key.category, &key.createdAt, &key.updatedAt, &key.retention); err != nil {
		return key, fmt.Errorf("scan passive key: %w", err)
	}
	key.pinned = pinned == 1
	clock := now.UTC().Truncate(time.Second)
	key.rank = key.importance * DecayFactor(key.category, key.retention, key.pinned, ageDays(key.createdAt, clock))
	return key, nil
}

// insertPassiveRankKey places one key into the window, which is held BEST FIRST
// and capped at capN. A key that belongs past the cap is dropped.
func insertPassiveRankKey(window []passiveRankKey, key passiveRankKey, order string, capN int) []passiveRankKey {
	at := sort.Search(len(window), func(i int) bool { return passiveRankKeyLess(key, window[i], order) })
	if at >= capN {
		return window
	}
	if len(window) >= capN {
		window = window[:capN-1]
	}
	window = append(window, passiveRankKey{})
	copy(window[at+1:], window[at:])
	window[at] = key
	return window
}

// passiveRankKeyStopsAt reports whether the scan may stop at key: whether key
// and every row after it rank below the window's last place.
//
// The scan reads rows in importance order, and within one importance value the
// index delivers them in the policy's secondary-key order (created_at for
// `decay`, updated_at for `pinned_importance_updated`), newest first. That
// ordering is what makes a non-strict stop sound: once the window is full, a row
// whose importance is below the window's k-th key cannot beat it (its rank is at
// most its importance), and a row whose importance EQUALS the k-th key arrives
// after every row in that tie that could beat the window on the secondary key,
// so it and the rest of the tie cannot enter the window either.
//
// The threshold is the policy's PRIMARY key, which is what the bound is about:
// `decay` ranks on the score, so the comparison is against the window's k-th
// score; `pinned_importance_updated` ranks on importance first, so it is against
// the k-th importance. An unread row's primary key is at most its importance
// (the score's because every decay factor is at most 1, importance's trivially),
// which is what makes the comparison sound.
func passiveRankKeyStopsAt(key, last passiveRankKey, order string) bool {
	if order == OrderPinnedImportanceUpdated {
		return key.importance <= last.importance
	}
	return key.importance <= last.rank
}

// passiveRankKeyLess is the policy's window order over scan keys, and it is the
// SQL ORDER BY the fetch used to state, moved into Go so the engine does not
// sort the bucket. It reports whether a ranks before b.
//
// The two policies genuinely differ, which is why the order travels as a value:
// `decay` ranks on the composite score, and `pinned_importance_updated` on
// pinned, then importance, then the most recently updated. Both lead with
// `pinned DESC`, and that is the half of the pin's slot guarantee that decides
// whether a pinned row is in the window AT ALL: a pinned row at importance 0.1
// ranks behind every 0.9 row the bucket holds, so a window of N rows full of
// better ones never contains it unless the order puts it first.
//
// Every key is the SQL key: `rank` is the Go mirror of the bound decay
// expression over the double the column holds, importance and created_at are
// compared as the store holds them, and `id` is the trailing tie-break. Naming
// `id` makes the window a function of the store's contents rather than of the
// order SQLite happened to produce — what a golden comparison across machines
// needs — and it can only REORDER rows left tied, never move a row past one it
// ranked.
func passiveRankKeyLess(a, b passiveRankKey, order string) bool {
	if a.pinned != b.pinned {
		return a.pinned
	}
	if order == OrderPinnedImportanceUpdated {
		if a.importance != b.importance {
			return a.importance > b.importance
		}
		if a.updatedAt != b.updatedAt {
			return a.updatedAt > b.updatedAt
		}
		return a.id < b.id
	}
	if a.rank != b.rank {
		return a.rank > b.rank
	}
	if a.importance != b.importance {
		return a.importance > b.importance
	}
	if a.createdAt != b.createdAt {
		return a.createdAt > b.createdAt
	}
	return a.id < b.id
}

// passiveRankKey is one row's rank facts, read by the threshold scan before the
// row is hydrated. It is a key rather than a Memory so the scan never carries a
// row's content, and it holds importance as the double the column stores so the
// tie it breaks is the engine's.
type passiveRankKey struct {
	id         string
	pinned     bool
	importance float64
	category   string
	createdAt  string
	updatedAt  string
	retention  string
	rank       float64
}

// hydratePassiveWindow reads the full rows of the window's keys, in the window's
// order. It is the one read that carries content, and it is bounded by the
// window: the bucket's rows are never materialised.
func (s *Store) hydratePassiveWindow(ctx context.Context, keys []passiveRankKey, cols passiveColumns) ([]Memory, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	marks := make([]string, len(keys))
	args := make([]any, len(keys))
	for i, k := range keys {
		marks[i] = "?"
		args[i] = k.id
	}
	rows, err := s.queryDB().QueryContext(ctx, "SELECT "+cols.list+" FROM memories WHERE id IN ("+strings.Join(marks, ",")+")", args...)
	if err != nil {
		return nil, fmt.Errorf("candidates: passive window hydration: %w", err)
	}
	defer func() { _ = rows.Close() }()
	memories, err := scanMemories(rows)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]Memory, len(memories))
	for _, m := range memories {
		byID[m.ID] = m
	}
	out := make([]Memory, 0, len(keys))
	for _, k := range keys {
		if m, ok := byID[k.id]; ok {
			out = append(out, m)
		}
	}
	return out, nil
}

// passiveKeyScanSQL builds one threshold scan's statement: the rank columns, the
// population, and the importance order the index serves. It does not limit or
// order by the policy's own order, because the window is kept in Go
// (scanPassiveKeys) — this statement only has to hand the rows over in
// importance order, which is what makes the stop condition sound.
//
// The validity predicate stays HERE, in SQL, rather than moving to the scan with
// the order. The window chooses which rows are kept, so a row whose validity
// window has closed must not spend one of its slots: judging it before it is
// offered to the window is what keeps the window full of rows the caller can
// use. It is GATED on the column existing, because on a store below the validity
// floor the predicate names columns that are not there and the whole fetch would
// fail with "no such column: valid_from".
//
// The bindings are in TEXT order: the bucket, then the validity stamps when the
// predicate is present.
func passiveKeyScanSQL(pol SlicePolicy, req CandidateRequest, cols passiveColumns, pinned bool) (string, []any) {
	where, args := passivePopulationSQL(pol, req, cols)
	if pinned {
		where += " AND pinned = 1"
	} else {
		where += " AND pinned = 0"
	}
	if cols.HasValidity {
		stamp := req.Now.UTC().Format(stampLayoutForSQL)
		where += " AND " + validityMatchesSQL()
		args = append(args, stamp, stamp)
	}
	retention := "retention"
	if !cols.HasTier {
		retention = "'' AS retention"
	}
	orderBy := "importance DESC, created_at DESC, id"
	if pol.Order == OrderPinnedImportanceUpdated {
		orderBy = "importance DESC, updated_at DESC, id"
	}
	query := fmt.Sprintf(`
		SELECT id, importance, pinned, category, created_at, updated_at, %s
		FROM memories
		WHERE %s
		ORDER BY %s`, retention, where, orderBy)
	return query, args
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

// passivePinnedBeyond counts the pinned rows the window left unread, by
// the row's own project (a union bucket reads `_global` rows too, and the cut
// belongs to the project the rows are shown under). The threshold scan reads the
// pinned population first and the window keeps pinned rows ahead of every
// unpinned one, so a pinned row can only be past the window when the whole window
// is pinned; any other window already holds every pinned row the population has,
// and the count is not run. It asks the same population and the same validity
// predicate the scan does, so a row the scan withholds is not counted as one the
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

// PassiveEligibleCountCap bounds the eligible-count aggregate. The count is
// HEADER ARITHMETIC — it says how many rows the store holds against the
// over-fetch's window — and it is the one read on this path the threshold scan
// cannot bound, because the rows the scan never read are exactly the ones the
// count is about. An exact census of a bucket past this many rows costs a table
// pass at every session start for a number a reader does not act on, so the
// aggregate stops here and reports that it did (capped), which makes the header
// say the bucket holds AT LEAST this many rows. It is exported so a test of a
// header that crosses it names the bound rather than repeating the number.
const PassiveEligibleCountCap = 10000

// PassiveEligibleCount is how many rows the policy's bucket holds that the
// window's fetch could ever draw from, and how many of those the fetch's own
// validity predicate removes before the window. Both come from ONE statement over
// passivePopulationSQL, the population the fetch and the validity probe already
// share, so the count and the window cannot disagree about which rows are in
// play and a validity-excluded row is counted once: inside `eligible`, and again
// as `validityExcluded`, never as a second row. `eligible - validityExcluded` is
// the number of rows the window could hold if it had no bound.
//
// A session-start header uses it to say how many rows the store holds against the
// over-fetch's window, so a bucket larger than the window does not read as if it
// held only the window, and rows the predicate withheld are reported as withheld
// rather than ranked out. A store below the validity floor has no predicate:
// validityExcluded is zero.
//
// The aggregate is CAPPED (PassiveEligibleCountCap): it reads at most that many
// rows, and a bucket that fills the cap reports capped with eligible equal to
// the cap, a LOWER BOUND, rather than paying a full table pass for an exact
// figure. validityExcluded is then the count over the rows the aggregate read,
// also a lower bound. A header built from a capped count says "at least", and
// never names the window as the total.
func (s *Store) PassiveEligibleCount(ctx context.Context, pol SlicePolicy, now time.Time, scope map[string]string) (eligible, validityExcluded int, capped bool, err error) {
	cols, err := passiveColumnsFor(s)
	if err != nil {
		return 0, 0, false, err
	}
	where, popArgs := passivePopulationSQL(pol, CandidateRequest{Scope: scope, Now: now}, cols)
	if !cols.HasValidity {
		query := "SELECT COUNT(*) FROM (SELECT 1 FROM memories WHERE " + where + " LIMIT ?)"
		args := append(append([]any{}, popArgs...), PassiveEligibleCountCap)
		if err := s.queryDB().QueryRowContext(ctx, query, args...).Scan(&eligible); err != nil {
			return 0, 0, false, fmt.Errorf("candidates: passive eligible count for bucket %q: %w", pol.Bucket, err)
		}
		return eligible, 0, eligible >= PassiveEligibleCountCap, nil
	}
	stamp := now.UTC().Format(stampLayoutForSQL)
	// The inner SELECT's validity placeholders come first in the statement's text,
	// then the population's bucket, then the cap.
	inner := "SELECT CASE WHEN " + validityMatchesSQL() + " THEN 0 ELSE 1 END AS invalid FROM memories WHERE " + where + " LIMIT ?"
	args := append([]any{stamp, stamp}, popArgs...)
	args = append(args, PassiveEligibleCountCap)
	query := "SELECT COUNT(*), COALESCE(SUM(invalid), 0) FROM (" + inner + ")"
	if err := s.queryDB().QueryRowContext(ctx, query, args...).Scan(&eligible, &validityExcluded); err != nil {
		return 0, 0, false, fmt.Errorf("candidates: passive eligible count for bucket %q: %w", pol.Bucket, err)
	}
	return eligible, validityExcluded, eligible >= PassiveEligibleCountCap, nil
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
	//
	// Every superseded row is NAMED (Candidate.SupersededBy), pinned or not, so the
	// assembler can mark it on its line: a live edge switches the lexical guess off
	// for its pair and resolve never retires a convention or a preference, so the
	// name is the only signal left. A keep-forever (retention-exempt) row is named
	// too, because the guess is off for it just the same, but the name is for the
	// marker ONLY. The demotion is never sunk by it (passiveDemote reads its own
	// edges with the exemption), and passivePinnedOnly leaves it out, so only a
	// PINNED, non-exempt superseded row drives the reservation and the arrangement;
	// every other superseded row reserves nothing and is sunk by the demotion, or
	// for an exempt one left in place, as it always was.
	named := s.passiveSupersededBy(ctx, eligible)
	supersededBy := passivePinnedOnly(named, eligible)
	chosen, rest, floor := passiveSelect(eligible, pol)
	chosen, rest = passiveReserve(chosen, rest, supersededBy)
	chosen, removed := s.passiveDemote(ctx, chosen, pol, now)
	// A demotion is a reorder and sinks a superseded row; the final arrangement
	// puts the reserved rows back at the head, in the order the demotion left them,
	// each superseded pin directly behind its superseder.
	chosen = passiveArrange(chosen, supersededBy)
	rest = append(rest, withheld...)

	out := make([]Candidate, 0, len(scored))
	for _, r := range append(chosen, rest...) {
		c := passiveCandidate(r, fetchedBy)
		c.SupersededBy = named[r.mem.ID]
		c.FloorReserved = floor[r.mem.ID]
		out = append(out, c)
	}
	var losers []DroppedLoser
	for _, l := range removed {
		losers = append(losers, DroppedLoser{Candidate: passiveCandidate(l.row, fetchedBy), LostTo: l.lostTo})
	}
	return out, losers, nil
}

// passiveSupersededBy reads, for each eligible row, pinned or not, the ids of the
// eligible rows that supersede it, sorted. The edge rules are the demotion's own
// (supersedePenaltyRows: a live edge only, the scope exemption) EXCEPT the
// persistent-retention exemption: a keep-forever target is never sunk, but it is
// named here, because a live edge switches the lexical guess off for its pair
// and a marker withholds nothing. The name is for the marker only; the slot
// rules read passivePinnedOnly, which leaves an exempt row out. A failed read is
// logged and means no row is named, which leaves the pins first and every row
// unmarked — the order and the lines before the marker existed.
func (s *Store) passiveSupersededBy(ctx context.Context, rows []passiveRow) map[string][]string {
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.mem.ID
	}
	against, err := supersedePenaltyRows(ctx, s.queryDB(), ids, nil)
	if err != nil {
		s.logger.Warn("candidates: passive supersede lookup failed", "error", err)
		return nil
	}
	out := map[string][]string{}
	for target, sources := range against {
		sorted := append([]string(nil), sources...)
		sort.Strings(sorted)
		out[target] = sorted
	}
	return out
}

// passivePinnedOnly is the subset of a passiveSupersededBy map whose targets are
// PINNED rows the demotion would sink (not retention-exempt): the one input the
// slot reservation (passiveReserve) and the arrangement (passiveArrange) read.
// Naming an unpinned or a keep-forever row must not widen the reservation or
// move a row, so the two maps are kept apart, and this subset is exactly the map
// the pinned path read before every row was named.
func passivePinnedOnly(named map[string][]string, rows []passiveRow) map[string][]string {
	out := map[string][]string{}
	for _, r := range rows {
		if srcs, ok := named[r.mem.ID]; ok && r.mem.Pinned && !RetentionExempt(r.mem) {
			out[r.mem.ID] = srcs
		}
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
//
// floor names the rows the behavioural floor reserved (Candidate.FloorReserved).
func passiveSelect(rows []passiveRow, pol SlicePolicy) (chosen, rest []passiveRow, floor map[string]bool) {
	if !pol.TwoPass || pol.BehaviorFloor <= 0 {
		return rows, nil, nil
	}
	poolCap := passivePoolCap(pol, len(rows))
	picked, floorN := passiveTwoPass(rows, pol, poolCap)
	floor = make(map[string]bool, floorN)
	for _, r := range picked[:floorN] {
		floor[r.mem.ID] = true
	}
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
	return picked, rest, floor
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

// passiveTwoPass returns the pool and how many of its leading rows the
// behavioural floor reserved.
func passiveTwoPass(rows []passiveRow, pol SlicePolicy, poolCap int) ([]passiveRow, int) {
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
	floorN := len(picked)
	for _, r := range rows {
		if len(picked) >= poolCap {
			break
		}
		if !used[r.mem.ID] {
			used[r.mem.ID] = true
			picked = append(picked, r)
		}
	}
	return picked, floorN
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
func (s *Store) passiveDemote(ctx context.Context, rows []passiveRow, pol SlicePolicy, now time.Time) ([]passiveRow, []passiveLoser) {
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
	// A protected row is never REMOVED as a near-duplicate. The verdict already
	// flips when exactly one side is protected, so a protected loser here means
	// BOTH rows of the pair are (two pinned copies, the pair a pinned fold
	// writes). The drop branch takes such a pair out of the verdicts before the
	// removal: both rows stay, nothing is filed as dropped, and no loser names a
	// winner it did not lose its slot to. The reorder branch keeps the pair, as
	// every reorder-only reader does, because a demotion keeps both rows.
	if err == nil && len(pairs) > 0 && pol.DropDemotedLosers {
		removable := make([]demotionPairs, 0, len(pairs))
		for _, pr := range pairs {
			if nearDupProtected[pr.loser] {
				continue
			}
			removable = append(removable, pr)
		}
		pairs = removable
	}
	// The contradicts veto is a DROP rule: a near-duplicate removal would take one
	// side out of the window before stage 5 sees it, so the pair would be judged
	// by the near-duplicate verdict instead of stage 5's contradiction tie-break,
	// and the survivor would carry no marker naming what it was separated from.
	// It is read only on the drop path for the same reason as the query path.
	if err == nil && len(pairs) > 0 && pol.DropDemotedLosers {
		var contradicted map[string]bool
		contradicted, err = contradictingIDs(ctx, s.queryDB(), ids)
		if err == nil && len(contradicted) > 0 {
			keptPairs := make([]demotionPairs, 0, len(pairs))
			for _, pr := range pairs {
				if contradicted[pr.loser] {
					continue
				}
				keptPairs = append(keptPairs, pr)
			}
			pairs = keptPairs
		}
	}
	// The lexical replacement exemption applies on BOTH branches. On the drop
	// branch a removal takes one row of the pair out before the read that names
	// the pair, so the block would hold one of the two claims with nothing saying
	// the other exists, and a failed read stops the removal, as the query path's
	// (dropNearDuplicates) does. On the reorder branch (a project bucket over its
	// cap) a demotion would push the newer row behind the cap, where
	// arrangeReplacements brings it back only into an unreserved slot; a failed
	// read there demotes as before, because the reorder keeps both rows.
	if err == nil && len(pairs) > 0 {
		mems := make([]Memory, len(rows))
		for i, r := range rows {
			mems[i] = r.mem
		}
		var exemptErr error
		pairs, exemptErr = exemptReplacementPairs(ctx, s.queryDB(), pairs, mems, now)
		if exemptErr != nil {
			if pol.DropDemotedLosers {
				err = exemptErr
			} else {
				s.logger.Warn("candidates: passive replacement pair lookup failed, demoting without the exemption", "error", exemptErr)
			}
		}
	}
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
	// triple (the first is the predicate in passiveKeyScanSQL), and
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
