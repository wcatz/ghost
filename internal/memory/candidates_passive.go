package memory

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

const (
	OrderDecay                   = "decay"
	OrderPinnedImportanceUpdated = "pinned_importance_updated"
)

const stampLayoutForSQL = "2006-01-02 15:04:05"

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
func validityMatchesSQL() string { return validityMatchesSQLFor("") }

func validityMatchesSQLFor(prefix string) string {
	until, from := readableStampSQL(prefix+"valid_until"), readableStampSQL(prefix+"valid_from")
	return "(" + prefix + "valid_until IS NULL OR NOT " + until + " OR " + stampComparableSQL(prefix+"valid_until") + " >= ?)" +
		" AND (" + prefix + "valid_from IS NULL OR NOT " + from + " OR " + stampComparableSQL(prefix+"valid_from") + " <= ?)"
}

const maxPassiveOverFetch = 200

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
	if mixesGlobal && seen[GlobalProjectID] {
		return fmt.Errorf("candidates: a policy admits %q into another bucket while a second policy fetches %q on its own; "+
			"the two row sets overlap, so every global row would be returned twice. Read them as two requests, or fetch "+
			"_global under the mixing bucket alone", GlobalProjectID, GlobalProjectID)
	}
	return nil
}

func (s *Store) candidatesPassive(ctx context.Context, req CandidateRequest, set *CandidateSet) (*CandidateSet, error) {
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
		fetched, losers, window, err := s.passiveBucket(ctx, req, pol, cols)
		if err != nil {
			return nil, err
		}
		if extra := len(fetched) + len(losers) - pol.OverFetch; extra > 0 {
			if set.WindowExtra == nil {
				set.WindowExtra = map[string]int{}
			}
			set.WindowExtra[pol.Bucket] = extra
		}
		set.DroppedLosers = append(set.DroppedLosers, losers...)
		// the set is non-empty and the assembler never reads this count. Probing the
		if len(fetched) == 0 {
			excluded, _, err := s.passiveValidityExcluded(ctx, req, pol, cols)
			if err != nil {
				return nil, err
			}
			set.ValidityExcluded += excluded
		}
		beyond, err := s.passivePinnedBeyond(ctx, req, pol, cols, window)
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
	// backfill supply), so the set is never larger than what was asked for. The

	scope := edgeScopeIDs(rows, len(rows))
	edges, status := s.loadCandidateEdges(ctx, scope)
	set.Edges, set.EdgesStatus = edges, status

	// read leaves the set as it was, unordered and unmarked: the guess must not be
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

	// zero is the honest answer for a store that has never recorded an
	counts := map[string]EvidenceCounts{}
	switch cols.evidenceReadMode() {
	case evidenceReadPlain:
		counts, err = evidenceCountsFor(ctx, s.queryDB(), scope)
		if err != nil {
			return nil, fmt.Errorf("candidates: evidence counts: %w", err)
		}
	case evidenceReadAttemptTolerating:
		// support, which is the one claim this must not invent. So it is attempted,
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
func (s *Store) passiveBucket(ctx context.Context, req CandidateRequest, pol SlicePolicy, cols passiveColumns) ([]Candidate, []DroppedLoser, []Memory, error) {
	memories, err := s.passiveWindow(ctx, req, pol, cols)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("candidates: passive fetch for bucket %q: %w", pol.Bucket, err)
	}
	window := memories
	replacements, err := s.passiveReplacementsOfPinned(ctx, req, pol, cols, memories)
	if err != nil {
		s.logger.Warn("candidates: passive replacement lookup for pinned rows failed", "error", err)
		replacements = nil
	}
	memories = append(memories, replacements...)
	selected, losers, err := s.selectPassive(ctx, memories, pol, req.Now, pol.Bucket)
	if err != nil {
		return nil, nil, nil, err
	}
	return selected, losers, window, nil
}

// passiveKeyScanVisited counts the rows the threshold scan has read since the
// last reset, and it exists so a test can pin the READ COUNT rather than a
// duration: the property the scan is for is that a bucket's low-ranked tail is
// never read, and that is a number, not a timing. It is process state rather
// than a field because the scan is a free function over a cursor and no caller
// should have to carry a counter it does not read.
var passiveKeyScanVisited atomic.Int64

// passiveWindow is a policy's window: the top OverFetch rows under the policy's
// own order, kept by a THRESHOLD SCAN rather than by sorting the bucket.
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

func (s *Store) scanPassiveKeys(ctx context.Context, req CandidateRequest, pol SlicePolicy, cols passiveColumns, pinned bool, window []passiveRankKey) ([]passiveRankKey, error) {
	maxDecay := s.passiveMaxDecay(ctx, req, pol, cols, pinned)
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
		if len(window) >= pol.OverFetch && passiveRankKeyStopsAt(key, window[len(window)-1], pol.Order, maxDecay) {
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
	var dated int
	if err := rows.Scan(&key.id, &key.importance, &pinned, &key.category, &dated, &key.createdAt, &key.updatedAt, &key.retention); err != nil {
		return key, fmt.Errorf("scan passive key: %w", err)
	}
	key.pinned = pinned == 1
	key.dated = dated == 1
	clock := now.UTC().Truncate(time.Second)
	key.rank = key.importance * decayFactorDated(key.category, key.dated, key.retention, key.pinned, ageDays(key.createdAt, clock))
	return key, nil
}

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

func passiveRankKeyStopsAt(key, last passiveRankKey, order string, maxDecay float64) bool {
	if order == OrderPinnedImportanceUpdated {
		return key.importance <= last.importance
	}
	return key.importance*maxDecay <= last.rank
}

func (s *Store) passiveMaxDecay(ctx context.Context, req CandidateRequest, pol SlicePolicy, cols passiveColumns, pinned bool) float64 {
	if pinned {
		return 1.0
	}
	if req.Now.IsZero() {
		return 1.0
	}
	buckets := []string{pol.Bucket}
	if pol.IncludeGlobal && pol.Bucket != GlobalProjectID {
		buckets = append(buckets, GlobalProjectID)
	}
	clock := req.Now.UTC().Truncate(time.Second)
	maxDecay := 0.0
	for _, bucket := range buckets {
		p := pol
		p.Bucket = bucket
		p.IncludeGlobal = false
		if d := s.passiveMaxDecayForBucket(ctx, p, cols, clock); d > maxDecay {
			maxDecay = d
		}
	}
	if maxDecay <= 0 {
		return 1.0
	}
	return maxDecay
}

// passiveMaxDecayForBucket is one bucket population's worst-case decay: the
// factor of the most favourable category present, at the population's youngest
// created_at. It answers by seeking, never scanning: the MAX picks the youngest
// instant out of the bucket (index seek) and the category probe picks the most
// favourable category that is there at all (index seek). Both fall back to the
// neutral factor when they cannot be read, which makes the bound looser rather
// than wrong — an unread row may then rank past the window, but never one that
// should have been read.
func (s *Store) passiveMaxDecayForBucket(ctx context.Context, pol SlicePolicy, cols passiveColumns, now time.Time) float64 {
	where, args := passivePopulationSQL(pol, CandidateRequest{}, cols)
	where += " AND pinned = 0"
	var newest string
	if err := s.queryDB().QueryRowContext(ctx, "SELECT COALESCE(MAX(created_at), '') FROM memories WHERE "+where, args...).Scan(&newest); err != nil {
		return 1.0
	}
	if newest == "" {
		return 0.0
	}
	age := ageDays(newest, now)
	var cat string
	if s.passiveCategoryPresent(ctx, where, args, []string{"preference", "convention", "fact"}) {
		cat = "preference"
	} else if s.passiveCategoryPresent(ctx, where, args, []string{"pattern", "architecture"}) {
		cat = "pattern"
	} else {
		cat = "gotcha"
	}
	return DecayFactor(cat, "", RetentionProject, false, age)
}

func (s *Store) passiveCategoryPresent(ctx context.Context, where string, args []any, categories []string) bool {
	if len(categories) == 0 {
		return false
	}
	placeholders := make([]string, len(categories))
	vals := make([]any, len(categories))
	for i, c := range categories {
		placeholders[i] = "?"
		vals[i] = c
	}
	q := "SELECT EXISTS(SELECT 1 FROM memories WHERE " + where + " AND category IN (" + strings.Join(placeholders, ",") + "))"
	var found int
	if err := s.queryDB().QueryRowContext(ctx, q, append(append([]any{}, args...), vals...)...).Scan(&found); err != nil {
		return false
	}
	return found == 1
}

// passiveRankKeyLess is the policy's window order over scan keys, and it is the
// SQL ORDER BY the fetch used to state, moved into Go so the engine does not
// sort the bucket. It reports whether a ranks before b.
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
// row is hydrated. It carries a dated flag, derived in SQL, because the rank's
// category-aware decay treats a dated fact differently from a timeless one, and
// it holds importance as the double the column stores so the tie it breaks is
// the engine's. The bucket's content is never carried into Go here, but the
// engine does read it: the GLOB behind the flag runs over `content` for each
// scanned row of category fact (the `category = 'fact' AND` guard short-circuits
// every other row), so the scan's read count, which counts rows, does not see
// that cost. It is accepted so the scan and every other rank share one rule.
type passiveRankKey struct {
	id         string
	pinned     bool
	importance float64
	category   string
	dated      bool
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
func passiveKeyScanSQL(pol SlicePolicy, req CandidateRequest, cols passiveColumns, pinned bool) (string, []any) {
	retention := "retention"
	if !cols.HasTier {
		retention = "'' AS retention"
	}
	orderBy := "importance DESC, created_at DESC, id"
	if pol.Order == OrderPinnedImportanceUpdated {
		orderBy = "importance DESC, updated_at DESC, id"
	}
	arm := func(bucket string) (string, []any) {
		one := pol
		one.Bucket = bucket
		one.IncludeGlobal = false
		where, args := passivePopulationSQL(one, req, cols)
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
		return fmt.Sprintf(`
		SELECT id, importance, pinned, category, (category = 'fact' AND content GLOB '*[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]*') AS dated, created_at, updated_at, %s
		FROM memories
		WHERE %s`, retention, where), args
	}
	query, args := arm(pol.Bucket)
	if pol.IncludeGlobal && pol.Bucket != GlobalProjectID {
		global, globalArgs := arm(GlobalProjectID)
		query += `
		UNION ALL` + global
		args = append(args, globalArgs...)
	}
	return query + `
		ORDER BY ` + orderBy, args
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

func (s *Store) passivePinnedBeyond(ctx context.Context, req CandidateRequest, pol SlicePolicy, cols passiveColumns, window []Memory) (map[string]int, error) {
	if len(window) < pol.OverFetch {
		return nil, nil
	}
	read := map[string]int{}
	for _, m := range window {
		if !m.Pinned {
			return nil, nil
		}
		read[m.ProjectID]++
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
func (s *Store) passiveValidityExcluded(ctx context.Context, req CandidateRequest, pol SlicePolicy, cols passiveColumns) (int, bool, error) {
	if !cols.HasValidity {
		return 0, false, nil
	}
	where, args := passivePopulationSQL(pol, req, cols)
	stamp := req.Now.UTC().Format(stampLayoutForSQL)
	args = append(args, stamp, stamp)
	inner := `SELECT 1 FROM memories WHERE ` + where + ` AND NOT (` + validityMatchesSQL() + `) LIMIT ?`
	args = append(args, PassiveEligibleCountCap)
	query := `SELECT COUNT(*) FROM (` + inner + `)`
	var n int
	if err := s.queryDB().QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, false, fmt.Errorf("candidates: passive validity exclusion count for bucket %q: %w", pol.Bucket, err)
	}
	return n, n >= PassiveEligibleCountCap, nil
}

func (s *Store) selectPassive(ctx context.Context, memories []Memory, pol SlicePolicy, now time.Time, fetchedBy string) ([]Candidate, []DroppedLoser, error) {
	scored := make([]passiveRow, 0, len(memories))
	for _, m := range memories {
		age := ageDays(m.CreatedAt, now)
		decay := DecayFactor(m.Category, m.Content, m.Retention, m.Pinned, age)
		scored = append(scored, passiveRow{
			mem:   m,
			age:   age,
			decay: decay,
			score: float64(m.Importance) * decay,
		})
	}
	// window is withheld by stage 2 whatever happens here, so it must not fill a
	eligible, withheld := passiveEligible(scored, now)
	// the tail and lead the set the demotions run over, so they are never outside
	named := s.passiveSupersededBy(ctx, eligible)
	supersededBy := passivePinnedOnly(named, eligible)
	chosen, rest, floor := passiveSelect(eligible, pol)
	chosen, rest = passiveReserve(chosen, rest, supersededBy)
	chosen, removed := s.passiveDemote(ctx, chosen, pol, now)
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
			at++
			for at < len(head) && len(supersededBy[head[at].mem.ID]) > 0 && containsString(supersededBy[head[at].mem.ID], head[at-1].mem.ID) {
				at++
			}
			head = append(head[:at], append([]passiveRow{r}, head[at:]...)...)
			placed = true
		}
		behind = still
		if !placed {
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

type passiveLoser struct {
	row    passiveRow
	lostTo []string
}

type passiveRow struct {
	mem   Memory
	age   float64
	decay float64
	score float64
}

// passiveTwoPass returns the window ordered with the behavioral reservation
// first.
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

func (s *Store) passiveDemote(ctx context.Context, rows []passiveRow, pol SlicePolicy, now time.Time) ([]passiveRow, []passiveLoser) {
	if len(rows) < 2 {
		return rows, nil
	}
	ids := make([]string, len(rows))
	// Pinned || RetentionExempt, so this must too or the two orderings disagree
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
	if penalty, err := SupersedePenalties(ctx, s.queryDB(), ids, supersedeProtected); err != nil {
		s.logger.Warn("candidates: passive supersede demotion lookup failed", "error", err)
	} else if len(penalty) > 0 {
		rows = StableDemote(rows, func(r passiveRow) string { return r.mem.ID }, penalty)
		for i, r := range rows {
			ids[i] = r.mem.ID
		}
	}
	if pol.DemoteOnlyWhenOverCap && len(rows) <= pol.ItemCap {
		return rows, nil
	}
	threshold := pol.DemotionThreshold
	if threshold <= 0 {
		s.mu.RLock()
		threshold = s.demotionThreshold
		s.mu.RUnlock()
	}
	pairs, err := nearDuplicatePenaltyRows(ctx, s.queryDB(), ids, nearDupProtected, threshold)
	// A protected row is never REMOVED as a near-duplicate. The verdict already
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

func passiveCandidate(r passiveRow, fetchedBy string) Candidate {
	c := Candidate{Memory: r.mem}
	c.Base = float64(r.mem.Importance)
	c.AgeDays = r.age
	c.Decay = r.decay
	c.Score = r.score
	c.FTSRank, c.VectorRank, c.VectorScore = -1, -1, -1
	c.FetchedBy = fetchedBy
	return c
}

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
	HasProvenance   bool
	ProvenanceKnown bool
}

const (
	passiveScopeColumnFloor      = 12
	passiveRetentionColumnFloor  = 19
	passiveValidityColumnFloor   = 10
	passiveProvenanceColumnFloor = 18
)

func passiveColumnsFor(s *Store) (passiveColumns, error) {
	var hasScope, hasTier, hasValidity, hasProvenance bool
	var versionKnown bool

	version, versionErr := dbUserVersion(s.queryDB())
	if versionErr == nil {
		versionKnown = true
		hasScope = version >= passiveScopeColumnFloor
		hasTier = version >= passiveRetentionColumnFloor
		hasProvenance = version >= passiveProvenanceColumnFloor
		hasValidity = version >= passiveValidityColumnFloor
	} else {
		s.logger.Warn("candidates: passive read could not read the store's schema version", "error", versionErr)
	}

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
func isMissingTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table")
}

type evidenceReadMode int

const (
	evidenceReadPlain evidenceReadMode = iota
	evidenceReadAttemptTolerating
	// evidenceReadSkip reports zero without reading, which is honest only when the
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
