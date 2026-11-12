package memory

import (
	"context"
	"fmt"
	"time"
)

// HistoryGrowthWindow is the window used to measure history growth.
const HistoryGrowthWindow = 24 * time.Hour

// HistoryNoOpShareWarn is the no-op share threshold that triggers a warning.
const HistoryNoOpShareWarn = 0.20

// HistoryCapHorizonDays is the cap horizon, in days, that triggers a warning.
const HistoryCapHorizonDays = 14

const (
	// HistoryWarnNoOpShare reports excessive no-op history rows.
	HistoryWarnNoOpShare = "no_op_share"
	// HistoryWarnPerMemoryCap reports a memory approaching its version cap.
	HistoryWarnPerMemoryCap = "per_memory_cap"
	// HistoryWarnStoreCap reports the store approaching its history row cap.
	HistoryWarnStoreCap = "store_cap"
)

// HistoryWarning describes one history-growth finding.
type HistoryWarning struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

// HistoryGrowthResult reports history-growth metrics and warnings.
type HistoryGrowthResult struct {
	WindowHours        int              `json:"window_hours"`
	RowsInWindow       int64            `json:"rows_in_window"`
	NoOpRows           int64            `json:"no_op_rows"`
	NoOpShare          float64          `json:"no_op_share"`
	MaxVersions        int64            `json:"max_versions"`
	CapMemoryID        string           `json:"cap_memory_id,omitempty"`
	CapMemoryVersions  int64            `json:"cap_memory_versions"`
	CapMemoryRecent    int64            `json:"cap_memory_recent"`
	PerMemoryCap       int64            `json:"per_memory_cap"`
	TotalRows          int64            `json:"total_rows"`
	StoreCap           int64            `json:"store_cap"`
	Projected          bool             `json:"projected"`
	DaysToPerMemoryCap float64          `json:"days_to_per_memory_cap"`
	DaysToStoreCap     float64          `json:"days_to_store_cap"`
	Warnings           []HistoryWarning `json:"warnings,omitempty"`
}

var historyGrowthAfterWindow func()

func historyWindowModifier() string {
	hours := int(HistoryGrowthWindow / time.Hour)
	return fmt.Sprintf("-%d hours", hours)
}

func historyWindowGrowthStmt() (string, []any) {
	sql := `SELECT count(*), COALESCE(sum(CASE WHEN ` + historyEqualPredecessorSQL("h") + ` THEN 1 ELSE 0 END), 0)
	    FROM memory_history h
	    WHERE h.recorded_at >= datetime('now', ?)`
	return sql, []any{historyWindowModifier()}
}

func historyPerMemoryGrowthStmt() (string, []any) {
	sql := `SELECT memory_id, count(*), COALESCE(sum(CASE WHEN recorded_at >= datetime('now', ?) THEN 1 ELSE 0 END), 0)
	    FROM memory_history
	    GROUP BY memory_id`
	return sql, []any{historyWindowModifier()}
}

// HistoryGrowth reports memory-history growth and retention pressure.
func (s *Store) HistoryGrowth(ctx context.Context) (HistoryGrowthResult, error) {
	res := HistoryGrowthResult{
		WindowHours:  int(HistoryGrowthWindow / time.Hour),
		PerMemoryCap: int64(historyVersionsPerMemory),
		StoreCap:     int64(historyRowsCap),
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, noOp, err := s.historyWindowCounts(ctx)
	if err != nil {
		return res, err
	}
	res.RowsInWindow, res.NoOpRows = rows, noOp
	if res.RowsInWindow > 0 {
		res.NoOpShare = float64(res.NoOpRows) / float64(res.RowsInWindow)
	}
	if historyGrowthAfterWindow != nil {
		historyGrowthAfterWindow()
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
		if c.recent == 0 {
			continue
		}
		days := float64(res.PerMemoryCap-c.total) / float64(c.recent)
		if !res.Projected || days < res.DaysToPerMemoryCap {
			res.DaysToPerMemoryCap = days
			res.CapMemoryID = c.id
			res.CapMemoryVersions = c.total
			res.CapMemoryRecent = c.recent
			res.Projected = true
		}
	}
	if res.RowsInWindow > 0 {
		res.DaysToStoreCap = float64(res.StoreCap-res.TotalRows) / float64(res.RowsInWindow)
	}

	res.Warnings = historyGrowthWarnings(res)
	return res, nil
}

func (s *Store) historyWindowCounts(ctx context.Context) (rows, noOp int64, err error) {
	query, args := historyWindowGrowthStmt()
	if err := s.queryDB().QueryRowContext(ctx, query, args...).Scan(&rows, &noOp); err != nil {
		return 0, 0, fmt.Errorf("count history versions in the window: %w", err)
	}
	return rows, noOp, nil
}

type memoryHistoryCounts struct {
	id     string
	total  int64
	recent int64
}

func (s *Store) historyPerMemoryCounts(ctx context.Context) ([]memoryHistoryCounts, error) {
	query, args := historyPerMemoryGrowthStmt()
	rows, err := s.queryDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read per-memory history counts: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var out []memoryHistoryCounts
	for rows.Next() {
		var c memoryHistoryCounts
		if err := rows.Scan(&c.id, &c.total, &c.recent); err != nil {
			return nil, fmt.Errorf("scan per-memory history counts: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate per-memory history counts: %w", err)
	}
	return out, nil
}

func historyGrowthWarnings(res HistoryGrowthResult) []HistoryWarning {
	if res.RowsInWindow == 0 {
		return nil
	}
	var warnings []HistoryWarning

	if res.NoOpShare > HistoryNoOpShareWarn {
		warnings = append(warnings, HistoryWarning{
			Kind: HistoryWarnNoOpShare,
			Detail: fmt.Sprintf(
				"%.0f%% of the %d version rows written in the last %dh restate the version before them (warning threshold %.0f%%) — they take up room without recording anything, and the retention caps are what evict them: `ghost history compact` reclaims pre-#727 restatements and deliberately leaves what this build wrote, plus the newest version of any memory",
				res.NoOpShare*100, res.RowsInWindow, res.WindowHours, HistoryNoOpShareWarn*100),
		})
	}

	if res.Projected && res.DaysToPerMemoryCap < HistoryCapHorizonDays {
		warnings = append(warnings, HistoryWarning{
			Kind:   HistoryWarnPerMemoryCap,
			Detail: historyPerMemoryCapDetail(res),
		})
	}

	if res.DaysToStoreCap < HistoryCapHorizonDays {
		warnings = append(warnings, HistoryWarning{
			Kind:   HistoryWarnStoreCap,
			Detail: historyStoreCapDetail(res),
		})
	}
	return warnings
}

func historyPerMemoryCapDetail(res HistoryGrowthResult) string {
	id := RefusalArg("memory_id", res.CapMemoryID)
	if res.DaysToPerMemoryCap <= 0 {
		return fmt.Sprintf(
			"memory %s is at the per-memory cap: it holds %d of its %d versions and wrote %d in the last %dh, so its oldest versions are what the trim takes now (warning threshold %d days)",
			id, res.CapMemoryVersions, res.PerMemoryCap, res.CapMemoryRecent,
			res.WindowHours, HistoryCapHorizonDays)
	}
	return fmt.Sprintf(
		"memory %s is closest to the per-memory cap: it holds %d of its %d versions and wrote %d in the last %dh, so the cap is %s away at that rate (warning threshold %d days)",
		id, res.CapMemoryVersions, res.PerMemoryCap, res.CapMemoryRecent,
		res.WindowHours, historyDaysText(res.DaysToPerMemoryCap), HistoryCapHorizonDays)
}

func historyStoreCapDetail(res HistoryGrowthResult) string {
	if res.DaysToStoreCap <= 0 {
		return fmt.Sprintf(
			"the store holds %d of its %d history rows and wrote %d in the last %dh, so it is at the store cap and the oldest rows in the table are what it trims — not the noisiest ones",
			res.TotalRows, res.StoreCap, res.RowsInWindow, res.WindowHours)
	}
	return fmt.Sprintf(
		"the store holds %d of its %d history rows and wrote %d in the last %dh — the store cap is %s away at that rate (warning threshold %d days); the oldest rows are what it trims",
		res.TotalRows, res.StoreCap, res.RowsInWindow, res.WindowHours,
		historyDaysText(res.DaysToStoreCap), HistoryCapHorizonDays)
}

func historyDaysText(days float64) string {
	switch {
	case days < 1:
		return "under a day"
	case days < 10:
		return fmt.Sprintf("%.1f days", days)
	default:
		return fmt.Sprintf("%.0f days", days)
	}
}
