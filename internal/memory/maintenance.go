package memory

import (
	"context"
	"database/sql"
	"fmt"
)

// MaintenanceRun is one recorded maintenance-hygiene event: today a scratch
// budget check (kind "scratch-budget") that fired before a harness spawn —
// over budget, so stale entries were reaped and possibly a loud warning was
// emitted. The row is what `ghost maintenance status` prints.
//
// RecordedAt is a single point-in-time timestamp rather than a started/finished
// pair: a budget check is instantaneous (measure → maybe reap → maybe warn),
// and scratch hygiene events have no meaningful duration. Kind is a free
// string so future hygiene kinds (reaps from other call sites, legacy cleanup)
// reuse the same table without another migration.
type MaintenanceRun struct {
	ID                 string `json:"id"`
	Kind               string `json:"kind"`
	RecordedAt         string `json:"recorded_at"`
	ScratchBytes       int64  `json:"scratch_bytes"`
	ScratchReapedBytes int64  `json:"scratch_reaped_bytes"`
	ScratchReapedCount int64  `json:"scratch_reaped_count"`
	Note               string `json:"note"`
}

// RecordMaintenanceRun inserts one hygiene event. The id and recorded_at
// timestamp come from the schema defaults (hex(randomblob(16)) /
// datetime('now')), so callers need only fill the payload fields.
func RecordMaintenanceRun(ctx context.Context, db *sql.DB, run MaintenanceRun) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO maintenance_runs
			(kind, scratch_bytes, scratch_reaped_bytes, scratch_reaped_count, note)
		 VALUES (?, ?, ?, ?, ?)`,
		run.Kind, run.ScratchBytes, run.ScratchReapedBytes, run.ScratchReapedCount, run.Note,
	)
	if err != nil {
		return fmt.Errorf("record maintenance run: %w", err)
	}
	return nil
}

// RecentMaintenanceRuns returns the most recent limit hygiene events,
// newest first — the order `ghost maintenance status` prints. A non-positive
// limit selects the default page.
func RecentMaintenanceRuns(ctx context.Context, db *sql.DB, limit int) ([]MaintenanceRun, error) {
	if limit <= 0 {
		limit = 20
	}
	var tableExists int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='maintenance_runs'`,
	).Scan(&tableExists); err != nil {
		return nil, fmt.Errorf("inspect maintenance runs table: %w", err)
	}
	if tableExists == 0 {
		return []MaintenanceRun{}, nil
	}
	rows, err := db.QueryContext(ctx,
		`SELECT id, kind, recorded_at, scratch_bytes, scratch_reaped_bytes, scratch_reaped_count, note
		 FROM maintenance_runs
		 ORDER BY recorded_at DESC, rowid DESC
		 LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list maintenance runs: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	runs := make([]MaintenanceRun, 0, limit)
	for rows.Next() {
		var r MaintenanceRun
		if err := rows.Scan(&r.ID, &r.Kind, &r.RecordedAt, &r.ScratchBytes,
			&r.ScratchReapedBytes, &r.ScratchReapedCount, &r.Note); err != nil {
			return nil, fmt.Errorf("scan maintenance run: %w", err)
		}
		runs = append(runs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list maintenance runs: %w", err)
	}
	return runs, nil
}
