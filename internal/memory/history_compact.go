package memory

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

var historyVersionColumns = []string{"content", "category", "importance", "resolved_at", "source"}

var historyEventColumns = []string{
	"memory_id", "project_id", "phase", "agent", "session_id", "related_id", "merged_content",
	"from_project_id", "to_project_id",
}

var historyRowColumns = []string{"id", "recorded_at"}

var historyCompactBatchSize = 500

func compactablePhases() []string {
	return []string{phaseReflect}
}

const reflectNoOpCutoff = "2026-09-28 17:14:07"

// ResolveCompactCutoff is the cutoff a run will use, normalized to the layout the
// comparison above is exact for. Exported because the report has to NAME the bound
// that produced its numbers: a count with an unstated bound is not a number an
// operator can reason about, and a bound they cannot see is a bound they cannot
// widen.
func ResolveCompactCutoff(before string) (string, error) {
	if before == "" {
		return reflectNoOpCutoff, nil
	}
	for _, layout := range []string{DateStampLayout, StoredStampLayout, time.RFC3339} {
		if at, err := time.Parse(layout, before); err == nil {
			return at.UTC().Format(StoredStampLayout), nil
		}
	}
	return "", fmt.Errorf("--before %q is neither a date (%s) nor an RFC 3339 instant",
		before, DateStampLayout)
}

// WidenedCompactCutoff reports whether a RESOLVED cutoff reaches rows that a
// current build wrote, which is what makes it wider than the default and what the
// command warns about before it is told to act on one.
func WidenedCompactCutoff(cutoff string) bool {
	return cutoff > reflectNoOpCutoff
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func historyEqualPredecessorSQL(outer string) string {
	eq := make([]string, 0, len(historyVersionColumns))
	for _, c := range historyVersionColumns {
		eq = append(eq, "p."+c+" IS "+outer+"."+c)
	}
	return "EXISTS (SELECT 1 FROM memory_history p WHERE p.rowid = (" +
		"SELECT max(q.rowid) FROM memory_history q WHERE q.memory_id = " + outer + ".memory_id AND q.rowid < " + outer + ".rowid" +
		") AND " + strings.Join(eq, " AND ") + ")"
}

func historyNewestVersionSQL(memoryID string) string {
	return "(SELECT max(n.rowid) FROM memory_history n WHERE n.memory_id = " + memoryID + ")"
}

func historyRemovableRowSQL(outer string) string {
	return historyRemovableLikeSQL(outer) +
		" AND " + outer + ".rowid <> " + historyNewestVersionSQL(outer+".memory_id") +
		" AND NOT " + tombstonedMemorySQL(outer+".memory_id")
}

var stampMovingPhases = []string{phaseSave, phaseUpdate, phaseReflect}

func historyRemovableLikeSQL(outer string) string {
	return historyEqualPredecessorSQL(outer) +
		" AND " + outer + ".phase IN (" + placeholders(len(compactablePhases())) + ")" +
		" AND " + outer + ".recorded_at < ?" +
		" AND " + outer + ".related_id IS NULL AND " + outer + ".merged_content IS NULL"
}

func tombstonedMemorySQL(memoryIDExpr string) string {
	return "COALESCE((SELECT t.phase FROM memory_history t WHERE t.rowid = " +
		historyNewestVersionSQL(memoryIDExpr) + "), '') = '" + phaseDelete + "'"
}

func compactDeleteStmt(projectID, cutoff string) (string, []any) {
	sql := `DELETE FROM memory_history WHERE rowid IN (
	    SELECT h.rowid FROM memory_history h
	    WHERE h.project_id = ? AND ` + historyRemovableRowSQL("h") + `
	    ORDER BY h.rowid
	    LIMIT ?)`
	return sql, append(compactPredicateArgs(projectID, cutoff), historyCompactBatchSize)
}

func compactCountStmt(projectID, cutoff string) (string, []any) {
	sql := `SELECT count(*) FROM memory_history h
	    WHERE h.project_id = ? AND ` + historyRemovableRowSQL("h")
	return sql, compactPredicateArgs(projectID, cutoff)
}

func compactCandidatesStmt(projectID, cursor, cutoff string) (string, []any) {

	evidence := `COALESCE((SELECT max(r.rowid) FROM memory_history r
	        WHERE r.memory_id = h.memory_id AND r.project_id = ? AND ` + historyRemovableRowSQL("r") + `), 0)`

	anchor := `(SELECT max(a.rowid) FROM memory_history a
	        WHERE a.memory_id = h.memory_id AND a.project_id = ?
	          AND NOT (` + historyRemovableLikeSQL("a") + `)
	          AND a.phase IN (` + placeholders(len(stampMovingPhases)) + `))`
	sql := `SELECT c.memory_id, c.target, COALESCE(t.recorded_at, ''), m.updated_at, c.removable_last
	    FROM (
	        SELECT h.memory_id AS memory_id, COALESCE(` + anchor + `, 0) AS target, ` + evidence + ` AS removable_last
	        FROM memory_history h
	        WHERE h.project_id = ? AND NOT ` + historyEqualPredecessorSQL("h") + `
	        GROUP BY h.memory_id
	    ) c
	    LEFT JOIN memory_history t ON t.rowid = c.target
	    JOIN memories m ON m.id = c.memory_id AND m.project_id = ?
	    WHERE (? = '' OR c.memory_id > ?)
	    ORDER BY c.memory_id
	    LIMIT ?`

	args := compactAnchorArgs(projectID, cutoff)
	args = append(args, compactPredicateArgs(projectID, cutoff)...)
	args = append(args, projectID, projectID, cursor, cursor, historyCompactBatchSize)
	return sql, args
}

func compactAnchorArgs(projectID, cutoff string) []any {
	args := make([]any, 0, len(compactablePhases())+len(stampMovingPhases)+2)
	args = append(args, projectID)
	for _, p := range compactablePhases() {
		args = append(args, p)
	}
	args = append(args, cutoff)
	for _, p := range stampMovingPhases {
		args = append(args, p)
	}
	return args
}

const historyCompactRestoreSQL = `UPDATE memories SET updated_at = ? WHERE id = ? AND project_id = ?`

func compactPredicateArgs(projectID, cutoff string) []any {
	args := make([]any, 0, len(compactablePhases())+2)
	args = append(args, projectID)
	for _, p := range compactablePhases() {
		args = append(args, p)
	}
	return append(args, cutoff)
}

// HistoryCompactOptions is what one project's compaction does.
type HistoryCompactOptions struct {
	Apply bool

	FixUpdatedAt bool

	Before string
}

// HistoryCompactResult is what one project's compaction found and did. The counts
// are the same numbers in a dry run and in an apply, which is what makes the dry
// run a preview rather than an estimate; see the fixed-point note on CompactHistory.
type HistoryCompactResult struct {
	ProjectID string `json:"project_id"`

	Before string `json:"before"`

	Removed int64 `json:"removed"`

	UpdatedAt int64 `json:"updated_at_restored"`

	StampsUnreadable int64 `json:"stamps_unreadable"`

	StampsUnrecorded int64 `json:"stamps_unrecorded"`
}

func (s *Store) CompactHistory(ctx context.Context, projectID string, opts HistoryCompactOptions) (HistoryCompactResult, error) {
	if projectID == "" {
		return HistoryCompactResult{}, fmt.Errorf("compact history: a project is required")
	}
	cutoff, err := ResolveCompactCutoff(opts.Before)
	if err != nil {
		return HistoryCompactResult{ProjectID: projectID}, err
	}
	res := HistoryCompactResult{ProjectID: projectID, Before: cutoff}

	if !opts.Apply {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.compactHistoryPreview(ctx, projectID, cutoff, opts, res)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if opts.FixUpdatedAt {
		fixed, unreadable, unrecorded, err := s.restoreUpdatedAt(ctx, projectID, cutoff, true)
		if err != nil {
			res.UpdatedAt, res.StampsUnreadable, res.StampsUnrecorded = fixed, unreadable, unrecorded
			return res, err
		}
		res.UpdatedAt, res.StampsUnreadable, res.StampsUnrecorded = fixed, unreadable, unrecorded
	}
	removed, err := s.deleteRemovableHistory(ctx, projectID, cutoff)
	if err != nil {
		res.Removed = removed
		return res, err
	}
	res.Removed = removed
	return res, nil
}

func (s *Store) compactHistoryPreview(ctx context.Context, projectID, cutoff string, opts HistoryCompactOptions,
	res HistoryCompactResult) (HistoryCompactResult, error) {
	if opts.FixUpdatedAt {
		fixed, unreadable, unrecorded, err := s.restoreUpdatedAt(ctx, projectID, cutoff, false)
		if err != nil {
			res.UpdatedAt, res.StampsUnreadable, res.StampsUnrecorded = fixed, unreadable, unrecorded
			return res, err
		}
		res.UpdatedAt, res.StampsUnreadable, res.StampsUnrecorded = fixed, unreadable, unrecorded
	}
	query, args := compactCountStmt(projectID, cutoff)
	var removed int64
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&removed); err != nil {
		return res, fmt.Errorf("count removable history versions: %w", err)
	}
	res.Removed = removed
	return res, nil
}

func (s *Store) deleteRemovableHistory(ctx context.Context, projectID, cutoff string) (int64, error) {
	query, args := compactDeleteStmt(projectID, cutoff)
	var total int64
	for {

		tx, lock, err := s.beginGuardedWrite(ctx, "history-compact")
		if err != nil {
			return total, fmt.Errorf("begin compact history: %w", err)
		}
		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			_ = tx.Rollback() //nolint:errcheck
			return total, fmt.Errorf("delete removable history versions: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			_ = tx.Rollback() //nolint:errcheck
			return total, fmt.Errorf("count deleted history versions: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return total, fmt.Errorf("commit compact history: %w", err)
		}

		lock.reportHold("history-compact", time.Now())
		total += n

		if n < int64(historyCompactBatchSize) {
			return total, nil
		}
	}
}

type compactStamp struct {
	memoryID      string
	target        int64
	recorded      string
	updatedAt     string
	removableLast int64
}

func restorableStamp(s compactStamp) (stamp string, restorable, unreadable, unrecorded bool) {
	if s.removableLast == 0 || s.removableLast <= s.target {
		return "", false, false, false
	}

	if s.target == 0 {
		return "", false, false, true
	}
	target, ok := ParseStamp(s.recorded)
	if !ok {
		return "", false, true, false
	}
	current, ok := ParseStamp(s.updatedAt)
	if !ok {
		return "", false, true, false
	}
	if !current.After(target) {
		return "", false, false, false
	}
	return target.UTC().Format(StoredStampLayout), true, false, false
}

func (s *Store) restoreUpdatedAt(ctx context.Context, projectID, cutoff string, apply bool) (fixed, unreadable, unrecorded int64, err error) {
	cursor := ""
	for {

		var (
			tx   *sql.Tx
			lock writeLock
		)
		if apply {

			if tx, lock, err = s.beginGuardedWrite(ctx, "history-compact-stamp"); err != nil {
				return fixed, unreadable, unrecorded, fmt.Errorf("begin restore updated_at: %w", err)
			}
		}
		var q stampQuerier = s.db
		if tx != nil {
			q = tx
		}
		batch, readErr := s.stampBatchQuery(ctx, q, projectID, cursor, cutoff)
		if readErr != nil {
			if tx != nil {
				_ = tx.Rollback() //nolint:errcheck
			}
			return fixed, unreadable, unrecorded, readErr
		}
		stamped := int64(0)
		for _, c := range batch {
			stamp, restorable, bad, absent := restorableStamp(c)
			if bad {
				unreadable++
			}
			if absent {
				unrecorded++
			}
			if !restorable {
				continue
			}
			stamped++
			if tx == nil {
				continue
			}
			if _, err := tx.ExecContext(ctx, historyCompactRestoreSQL, stamp, c.memoryID, projectID); err != nil {
				_ = tx.Rollback() //nolint:errcheck
				return fixed, unreadable, unrecorded, fmt.Errorf("restore updated_at of %s: %w", RefusalArg("memory_id", c.memoryID), err)
			}
		}
		if tx != nil {
			if err := tx.Commit(); err != nil {
				return fixed, unreadable, unrecorded, fmt.Errorf("commit restore updated_at: %w", err)
			}
			lock.reportHold("history-compact-stamp", time.Now())
		}
		fixed += stamped
		if len(batch) < historyCompactBatchSize {
			return fixed, unreadable, unrecorded, nil
		}
		cursor = batch[len(batch)-1].memoryID
	}
}

func (s *Store) stampBatchQuery(ctx context.Context, q stampQuerier, projectID, cursor, cutoff string) ([]compactStamp, error) {
	query, args := compactCandidatesStmt(projectID, cursor, cutoff)
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read memories to restore updated_at: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var batch []compactStamp
	for rows.Next() {
		var c compactStamp
		if err := rows.Scan(&c.memoryID, &c.target, &c.recorded, &c.updatedAt, &c.removableLast); err != nil {
			return nil, fmt.Errorf("scan memory to restore updated_at: %w", err)
		}
		batch = append(batch, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate memories to restore updated_at: %w", err)
	}
	return batch, nil
}

type stampQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}
