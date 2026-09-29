package memory

import (
	"context"
	"fmt"
	"time"
)

// Issue #729, reporting #730's damage. `ghost history compact` can remove the
// version rows that restate the version before them, but a repair nobody can see
// coming is a repair run too late: memory_history fills silently, and the two
// retention caps that bound it (historyVersionsPerMemory, historyRowsCap) trim
// oldest-first, so what disappears when the table fills is the OLDEST real
// change, not the noise. HistoryGrowth is the read that makes it visible before
// that happens, and it is the same read for `ghost mcp status` and ghost_health
// so the two cannot disagree about a store.
//
// It answers four questions, all of them about the table as a writer finds it:
//
//   - how many version rows were recorded in the last day, and how many of them
//     restated the row before them (the no-op share — the fraction of the table
//     the lifecycle is spending on saying nothing);
//   - how many versions the busiest memory holds, against the per-memory cap;
//   - how many rows the table holds, against the store cap;
//   - and, from the same 24-hour rate, when the second and third of those arrive.
//
// Two things this deliberately is NOT:
//
// A SECOND COPY OF THE EQUALITY PREDICATE. The numerator is
// historyEqualPredecessorSQL — the same function the compaction's delete, its
// dry-run count and its updated_at pass all use — and not a spelling of its own.
// A second predicate would be a second answer to "what counts as a restatement",
// and the report would then be free to disagree with the command that fixes what
// it reports. The relationship between the two numbers is spelled out on
// HistoryGrowth and tested by
// TestHistoryGrowthNoOpShareAgreesWithWhatCompactWouldRemove: the report's share
// counts every restatement, while the repair removes a SUBSET of them (a
// memory's newest version is what it says now, and a row naming another memory
// is a thread rather than a restatement), so the report over-counts on purpose —
// it is measuring how much noise the table carries, not how much is disposable.
//
// A SECOND SAYING OF ANY OF ITS OWN ADVICE. Every warning carries a Detail
// string built here, and both surfaces print that string rather than composing
// their own sentence from the numbers: the threshold a store tripped and the
// command that repairs it are the same words in `ghost mcp status` and in
// ghost_health, because they are written once.

// HistoryGrowthWindow is how far back the rate is measured. A day because the
// question is about a store being USED — how much the lifecycle writes on an
// ordinary run of it — and because a shorter window is a sample of a single
// lifecycle pass rather than of a day's worth of them.
const HistoryGrowthWindow = 24 * time.Hour

// HistoryNoOpShareWarn is the restatement share above which the report warns.
//
// Above a fifth rather than at any non-zero share, because a small amount of
// restatement is the ordinary shape of a working store and a threshold that
// fires on it teaches an operator to ignore the line. What makes restatement
// harmful is volume: it is a majority of the table's growth that pushes real
// events out from under the per-memory cap and then, at the store cap, out of the
// table entirely.
const HistoryNoOpShareWarn = 0.20

// HistoryCapHorizonDays is how close a cap has to be, in days at the current
// rate, before the report says so.
//
// A fortnight because the answer has to be actionable rather than alarming: a
// store that would fill its table in a year is not a store with a problem, and a
// report that names one every time it runs is a report nobody reads. Two weeks is
// long enough to compact at leisure, and short enough that the warning is not
// the first notice an operator gets of a table that has been full for a month.
const HistoryCapHorizonDays = 14

// The warning kinds. A fixed vocabulary rather than free text, because the kind
// is what a consumer matches on and the Detail is what a human reads; a caller
// that had to match on prose would break the first time the sentence was
// reworded.
const (
	// HistoryWarnNoOpShare: too much of the recent history restated the row
	// before it. The only warning with a repair attached.
	HistoryWarnNoOpShare = "no_op_share"
	// HistoryWarnPerMemoryCap: a memory will reach the per-memory cap soon, and
	// reaching it means its oldest versions — which may be the only record of
	// what it said first — start being trimmed.
	HistoryWarnPerMemoryCap = "per_memory_cap"
	// HistoryWarnStoreCap: the table will reach the store cap soon, and reaching
	// it trims the OLDEST rows in the table, not the noisiest ones.
	HistoryWarnStoreCap = "store_cap"
)

// HistoryWarning is one finding, carrying its own sentence.
type HistoryWarning struct {
	// Kind is one of the HistoryWarn* constants.
	Kind string `json:"kind"`
	// Detail is the sentence to show a person: the number that tripped the
	// warning, the threshold it tripped against, and where relevant the command
	// that acts on it. Written here so both surfaces render the same words.
	Detail string `json:"detail"`
}

// HistoryGrowthResult is what one store's history looks like to a reader.
//
// The counts are deliberately in two different clocks and never blended: the
// share, the rate and both projections are about the window, while TotalRows and
// MaxVersions are about the table as it stands. A row recorded three days ago is
// a no-op for the cap pressure and not for the rate, and the report that
// conflated the two would warn about a rate the store is not writing at.
type HistoryGrowthResult struct {
	// WindowHours is the window's length in hours, carried so a consumer can
	// name the window it is reporting without repeating HistoryGrowthWindow.
	WindowHours int `json:"window_hours"`
	// RowsInWindow is how many version rows were recorded in the window.
	RowsInWindow int64 `json:"rows_in_window"`
	// NoOpRows is how many of those recorded exactly the state the row before
	// them of the same memory recorded.
	NoOpRows int64 `json:"no_op_rows"`
	// NoOpShare is NoOpRows over RowsInWindow, or 0 on an empty window rather
	// than a division by zero.
	NoOpShare float64 `json:"no_op_share"`
	// BusiestMemoryRows is the most version rows any ONE memory recorded in the
	// window — the rate of the memory under the most pressure, which is not
	// necessarily the memory with the most versions.
	BusiestMemoryRows int64 `json:"busiest_memory_rows"`
	// MaxVersions is the most version rows any one memory holds, over the whole
	// table. This is the number the per-memory cap is measured against.
	MaxVersions int64 `json:"max_versions"`
	// PerMemoryCap is historyVersionsPerMemory, carried from the policy so a
	// report can never state a cap the store is not enforcing.
	PerMemoryCap int64 `json:"per_memory_cap"`
	// TotalRows is how many rows the table holds, over the whole table. This is
	// the number the store cap is measured against.
	TotalRows int64 `json:"total_rows"`
	// StoreCap is historyRowsCap, carried from the policy for the same reason.
	StoreCap int64 `json:"store_cap"`
	// Projected reports whether the two projections below are meaningful: they
	// are a rate extrapolated, and a store with no rows in the window has no
	// rate to extrapolate.
	Projected bool `json:"projected"`
	// DaysToPerMemoryCap is how long until some memory reaches the per-memory
	// cap at its own 24-hour rate. Zero when Projected is false.
	DaysToPerMemoryCap float64 `json:"days_to_per_memory_cap"`
	// DaysToStoreCap is how long until the table reaches the store cap at the
	// store's 24-hour rate. Zero when Projected is false.
	DaysToStoreCap float64 `json:"days_to_store_cap"`
	// Warnings is empty on a store with nothing to say.
	Warnings []HistoryWarning `json:"warnings,omitempty"`
}

// historyWindowModifier is the window as a SQLite date modifier, and the ONE
// place the window is spelled. Two statements read the same rows, and a filter
// that meant a different window in each would produce a share whose denominator
// is not the set its numerator was counted over — which is a report that looks
// right and is not.
func historyWindowModifier() string {
	hours := int(HistoryGrowthWindow / time.Hour)
	return fmt.Sprintf("-%d hours", hours)
}

// historyWindowGrowthStmt counts the rows in the window and how many of them
// restated their predecessor, in ONE pass, because the share is a ratio of two
// counts over the same rows: two statements would be two scans and two chances to
// read a different set of rows while a writer is appending between them.
//
// The filter cannot use an index, and that is the accepted cost rather than an
// oversight. recorded_at is the SECOND column of idx_history_memory, so the only
// plan available for "recorded in the last day" is a pass over the table — and
// the pass is bounded, because pruneHistoryTx holds the table at historyRowsCap
// on the write path of every save. The alternative, a standalone recorded_at
// index, is refused in schema.go with a measurement: it costs a fifth of the cost
// of writing the history row at all, permanently, on the write path, to save the
// couple of milliseconds this read spends on a full table. A status line is not
// where that trade belongs.
//
// The restatement test is historyEqualPredecessorSQL, which is a correlated
// sub-select: one index seek to the row before this one, and one seek to this
// memory's versions. TestHistoryGrowthQueryPlanIsOnePass pins that shape, because
// it is the difference between milliseconds and minutes on a full table.
func historyWindowGrowthStmt() (string, []any) {
	sql := `SELECT count(*), COALESCE(sum(CASE WHEN ` + historyEqualPredecessorSQL("h") + ` THEN 1 ELSE 0 END), 0)
	    FROM memory_history h
	    WHERE h.recorded_at >= datetime('now', ?)`
	return sql, []any{historyWindowModifier()}
}

// historyPerMemoryGrowthStmt returns one row per memory: how many versions it
// holds in total, and how many it wrote in the window.
//
// Both answers, and the projection, come from this one statement rather than
// from three, because the projection is PER MEMORY: a memory's days to its cap is
// what it has left divided by what it wrote, and a report that measured the
// per-memory cap against the store-wide rate would name a memory that is not the
// one about to be trimmed. Computing that in Go over one row per memory keeps the
// per-memory arithmetic out of SQL, where the same number would have to be
// expressed twice (once for the cap, once for nothing else).
//
// It is a covering index scan: idx_history_memory carries memory_id (the GROUP
// BY key) and recorded_at (the window test), so the whole pass is served from the
// index and the table itself is never touched. The result is one row per memory,
// which is the only place a read here is bounded by something other than the row
// cap, and it is bounded by the number of memories rather than the number of
// versions.
func historyPerMemoryGrowthStmt() (string, []any) {
	sql := `SELECT count(*), COALESCE(sum(CASE WHEN recorded_at >= datetime('now', ?) THEN 1 ELSE 0 END), 0)
	    FROM memory_history
	    GROUP BY memory_id`
	return sql, []any{historyWindowModifier()}
}

// HistoryGrowth reports how fast memory_history is growing into its retention
// caps, and how much of that growth is version rows that changed nothing.
//
// Read-only and bounded: two statements, one pass each, the aggregate over the
// window and the per-memory counts over the table (see the two functions above
// for why neither can use an index and why that is the right trade). It is safe
// to call while the lifecycle is writing, and it takes no transaction — nothing
// is written, so there is no write lock to take, for the same reason
// compactHistoryPreview opens none.
//
// The store's caps are store-wide (pruneHistoryTx ranks by rowid over the whole
// table), so the report is store-wide too. A per-project number would be a
// different question asked of a policy that does not answer per project: a table
// fills as one table, and the memory trimmed by the per-memory cap is trimmed
// whoever owns the project.
func (s *Store) HistoryGrowth(ctx context.Context) (HistoryGrowthResult, error) {
	res := HistoryGrowthResult{
		WindowHours:  int(HistoryGrowthWindow / time.Hour),
		PerMemoryCap: int64(historyVersionsPerMemory),
		StoreCap:     int64(historyRowsCap),
	}

	query, args := historyWindowGrowthStmt()
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&res.RowsInWindow, &res.NoOpRows); err != nil {
		return res, fmt.Errorf("count history versions in the window: %w", err)
	}
	if res.RowsInWindow > 0 {
		res.NoOpShare = float64(res.NoOpRows) / float64(res.RowsInWindow)
	}

	counts, err := s.historyPerMemoryCounts(ctx)
	if err != nil {
		return res, err
	}
	for _, c := range counts {
		res.TotalRows += c.total
		if c.total > res.MaxVersions {
			res.MaxVersions = c.total
		}
		if c.recent > res.BusiestMemoryRows {
			res.BusiestMemoryRows = c.recent
		}
		// The soonest memory wins the projection, and each is measured against
		// its OWN count: what a memory has left of the cap, over what it wrote
		// in the window. A memory that wrote nothing in the window is left out
		// entirely rather than counted as "infinitely far from the cap" — it is
		// not moving, and the report is about what is moving.
		if c.recent == 0 {
			continue
		}
		days := float64(res.PerMemoryCap-c.total) / float64(c.recent)
		if !res.Projected || days < res.DaysToPerMemoryCap {
			res.DaysToPerMemoryCap = days
			res.Projected = true
		}
	}
	if res.RowsInWindow > 0 {
		res.DaysToStoreCap = float64(res.StoreCap-res.TotalRows) / float64(res.RowsInWindow)
	}

	res.Warnings = historyGrowthWarnings(res)
	return res, nil
}

// memoryHistoryCounts is one memory's share of the table: every version it holds,
// and how many of them were recorded in the window.
type memoryHistoryCounts struct {
	total  int64
	recent int64
}

// historyPerMemoryCounts reads the per-memory statement, accumulating in the
// caller rather than collecting the rows here: a store can hold a memory per row
// in memories, and a status line has no business materialising that list when the
// answer is three maxima.
func (s *Store) historyPerMemoryCounts(ctx context.Context) ([]memoryHistoryCounts, error) {
	// The read lock, for the reason every other reader here takes it: a
	// consistent snapshot of the table across the two statements. Not a
	// transaction — a writer waiting behind a status line for the length of two
	// scans is the wrong trade for a report — and this is the same shape
	// CompactHistory's preview uses on the pool.
	s.mu.RLock()
	defer s.mu.RUnlock()

	query, args := historyPerMemoryGrowthStmt()
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read per-memory history counts: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var out []memoryHistoryCounts
	for rows.Next() {
		var c memoryHistoryCounts
		if err := rows.Scan(&c.total, &c.recent); err != nil {
			return nil, fmt.Errorf("scan per-memory history counts: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate per-memory history counts: %w", err)
	}
	return out, nil
}

// historyGrowthWarnings is the whole policy, in one place, and it is a function of
// the result rather than of anything read twice: both surfaces call it, and a
// threshold that lived beside one of them would be a threshold only that surface
// enforced.
//
// Every branch names the number that tripped it AND the threshold it tripped
// against. A warning that says only "too much history" leaves the reader to guess
// whether they are one row over or an order of magnitude past, which is the
// difference between running the repair now and running it after reading the
// rest of the report.
func historyGrowthWarnings(res HistoryGrowthResult) []HistoryWarning {
	if res.RowsInWindow == 0 {
		// Nothing was written in the window, so there is no rate, no share and
		// nothing to project. A store nobody has written to is not a store with a
		// problem, and this is the report every fresh install runs first.
		return nil
	}
	var warnings []HistoryWarning

	if res.NoOpShare > HistoryNoOpShareWarn {
		warnings = append(warnings, HistoryWarning{
			Kind: HistoryWarnNoOpShare,
			Detail: fmt.Sprintf(
				"%.0f%% of the %d version rows written in the last %dh restate the version before them (warning threshold %.0f%%) — run `ghost history compact` to remove them",
				res.NoOpShare*100, res.RowsInWindow, res.WindowHours, HistoryNoOpShareWarn*100),
		})
	}

	if res.Projected && res.DaysToPerMemoryCap < HistoryCapHorizonDays {
		warnings = append(warnings, HistoryWarning{
			Kind: HistoryWarnPerMemoryCap,
			Detail: fmt.Sprintf(
				"the busiest memory holds %d of its %d versions and wrote %d in the last %dh — it reaches the per-memory cap in %s at that rate (warning threshold %d days)",
				res.MaxVersions, res.PerMemoryCap, res.BusiestMemoryRows, res.WindowHours,
				historyDaysText(res.DaysToPerMemoryCap), HistoryCapHorizonDays),
		})
	}

	if res.DaysToStoreCap < HistoryCapHorizonDays {
		warnings = append(warnings, HistoryWarning{
			Kind: HistoryWarnStoreCap,
			Detail: fmt.Sprintf(
				"the store holds %d of its %d history rows and wrote %d in the last %dh — the store cap is %s away at that rate (warning threshold %d days); the oldest rows are what it trims",
				res.TotalRows, res.StoreCap, res.RowsInWindow, res.WindowHours,
				historyDaysText(res.DaysToStoreCap), HistoryCapHorizonDays),
		})
	}
	return warnings
}

// historyDaysText renders a projection for a person. The days are only ever
// printed inside a warning, where the value is under the horizon and one decimal
// is the difference between "3.2 days" and a number too round to trust; past
// that the decimal is noise on a figure nobody is acting on.
func historyDaysText(days float64) string {
	switch {
	case days <= 0:
		return "already there"
	case days < 1:
		return "under a day"
	case days < 10:
		return fmt.Sprintf("%.1f days", days)
	default:
		return fmt.Sprintf("%.0f days", days)
	}
}
