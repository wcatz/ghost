package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// DefaultPruneGrace is DefaultSessionGrace, spelled as its own name so the
// command's default and the tier's documented margin cannot be read as two
// different numbers.
const DefaultPruneGrace = DefaultSessionGrace

var errPruneInterrupted = errors.New("prune: interrupted before the delete")

var pruneBeforeDelete func() error

// PruneOptions is what one prune run is doing. The zero value is a dry run over
// every project with the documented grace, which is the shape a caller gets for
// asking nothing.
type PruneOptions struct {
	Apply     bool
	Grace     time.Duration
	Now       time.Time
	ProjectID string
}

// PruneCandidate is one row a prune would remove, with everything the report
// needs to be checkable by a reader: which tier it is, when it expires, when
// anything last touched it, and what the grace is measured from. The text is
// included because a report of rows the operator cannot recognise is a report
// they cannot approve.
type PruneCandidate struct {
	ID         string
	ProjectID  string
	Category   string
	Content    string
	Retention  string
	ExpiresAt  string
	ActivityAt string
	GraceFrom  string
}

// PruneReport is what one run did, or would do.
type PruneReport struct {
	Applied    bool
	Grace      time.Duration
	Now        time.Time
	ProjectID  string
	Candidates []PruneCandidate
	RemovedIDs []string
	Removed    int
}

const pruneActivitySQL = "COALESCE(last_accessed, max(updated_at, created_at, expires_at))"

const prunePredicate = `
		retention = '` + RetentionSession + `'
		AND pinned = 0
		AND expires_at IS NOT NULL
		AND expires_at <= ?
		AND datetime(` + pruneActivitySQL + `) <= datetime(?)
`

const pruneBatchSize = 500

func (s *Store) PruneSessionMemories(ctx context.Context, opts PruneOptions) (PruneReport, error) {
	if opts.Grace < 0 {
		return PruneReport{}, fmt.Errorf("invalid grace period %s: it must be zero (the %s default) or a positive duration",
			opts.Grace, DefaultPruneGrace)
	}
	grace := opts.Grace
	if grace == 0 {
		grace = DefaultPruneGrace
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	report := PruneReport{Applied: opts.Apply, Grace: grace, Now: now, ProjectID: opts.ProjectID}

	predArgs := []any{stampForCompare(now), stampForCompare(now.Add(-grace))}
	scope := ""
	if opts.ProjectID != "" {
		scope = "\n\t\t  AND project_id = ?"
		predArgs = append(predArgs, opts.ProjectID)
	}
	order := "\n\t\tORDER BY " + pruneActivitySQL + ", id"

	candQuery := `
		SELECT id, project_id, category, content, retention, expires_at,
		       COALESCE(last_accessed, updated_at, created_at),
		       ` + pruneActivitySQL + `
		FROM memories
		WHERE ` + prunePredicate + scope + order

	s.mu.RLock()
	rows, err := s.db.QueryContext(ctx, candQuery, predArgs...)
	if err != nil {
		s.mu.RUnlock()
		return report, fmt.Errorf("find prunable memories: %w", err)
	}
	for rows.Next() {
		var c PruneCandidate
		if err := rows.Scan(&c.ID, &c.ProjectID, &c.Category, &c.Content, &c.Retention, &c.ExpiresAt, &c.ActivityAt, &c.GraceFrom); err != nil {
			rows.Close() //nolint:errcheck
			s.mu.RUnlock()
			return report, fmt.Errorf("scan prunable memory: %w", err)
		}
		report.Candidates = append(report.Candidates, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close() //nolint:errcheck
		s.mu.RUnlock()
		return report, fmt.Errorf("iterate prunable memories: %w", err)
	}
	if err := rows.Close(); err != nil {
		s.mu.RUnlock()
		return report, fmt.Errorf("close prunable memories: %w", err)
	}
	s.mu.RUnlock()

	if !opts.Apply || len(report.Candidates) == 0 {
		return report, nil
	}

	batchQuery := `
		SELECT id
		FROM memories
		WHERE ` + prunePredicate + scope + order + "\n\t\tLIMIT ?"

	for {
		s.mu.Lock()
		tx, lock, err := s.beginGuardedWrite(ctx, "prune batch")
		if err != nil {
			s.mu.Unlock()
			return report, fmt.Errorf("begin prune batch: %w", err)
		}

		batchArgs := append(append([]any{}, predArgs...), pruneBatchSize)
		idRows, err := tx.QueryContext(ctx, batchQuery, batchArgs...)
		if err != nil {
			tx.Rollback() //nolint:errcheck
			s.mu.Unlock()
			return report, fmt.Errorf("find prune batch: %w", err)
		}
		ids := make([]string, 0, pruneBatchSize)
		for idRows.Next() {
			var id string
			if err := idRows.Scan(&id); err != nil {
				idRows.Close() //nolint:errcheck
				tx.Rollback()  //nolint:errcheck
				s.mu.Unlock()
				return report, fmt.Errorf("scan prune batch: %w", err)
			}
			ids = append(ids, id)
		}
		if err := idRows.Err(); err != nil {
			idRows.Close() //nolint:errcheck
			tx.Rollback()  //nolint:errcheck
			s.mu.Unlock()
			return report, fmt.Errorf("iterate prune batch: %w", err)
		}
		if err := idRows.Close(); err != nil {
			tx.Rollback() //nolint:errcheck
			s.mu.Unlock()
			return report, fmt.Errorf("close prune batch: %w", err)
		}
		if len(ids) == 0 {
			if err := tx.Commit(); err != nil {
				s.mu.Unlock()
				return report, fmt.Errorf("commit empty prune batch: %w", err)
			}
			s.mu.Unlock()
			break
		}

		if err := appendHistoryForIDsTx(ctx, tx, ids, phaseDelete, Provenance{}); err != nil {
			tx.Rollback() //nolint:errcheck
			s.mu.Unlock()
			return report, err
		}

		if pruneBeforeDelete != nil {
			if err := pruneBeforeDelete(); err != nil {
				tx.Rollback() //nolint:errcheck
				s.mu.Unlock()
				return report, err
			}
		}

		placeholders := make([]string, len(ids))
		delArgs := make([]any, len(ids))
		for i, id := range ids {
			placeholders[i] = "?"
			delArgs[i] = id
		}
		del := `DELETE FROM memories WHERE id IN (` + strings.Join(placeholders, ",") + `)`
		res, err := tx.ExecContext(ctx, del, delArgs...)
		if err != nil {
			tx.Rollback() //nolint:errcheck
			s.mu.Unlock()
			return report, fmt.Errorf("remove pruned memories: %w", err)
		}
		removed, err := res.RowsAffected()
		if err != nil {
			tx.Rollback() //nolint:errcheck
			s.mu.Unlock()
			return report, fmt.Errorf("count pruned memories: %w", err)
		}
		if int(removed) != len(ids) {
			tx.Rollback() //nolint:errcheck
			s.mu.Unlock()
			return report, fmt.Errorf("prune removed %d rows but recorded %d tombstones in one batch: the delete and the audit disagree", removed, len(ids))
		}

		if err := tx.Commit(); err != nil {
			s.mu.Unlock()
			return report, fmt.Errorf("commit prune batch: %w", err)
		}
		s.mu.Unlock()
		lock.reportHold("prune batch", time.Now())

		report.RemovedIDs = append(report.RemovedIDs, ids...)
		report.Removed += int(removed)
	}
	return report, nil
}

func stampForCompare(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05") }
