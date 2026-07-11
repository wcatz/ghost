package obsidian

import (
	"context"
	"database/sql"
	"time"
)

// Sync mirrors once immediately, then re-exports whenever PRAGMA data_version
// reports a commit from another connection. Runs until ctx is cancelled.
//
// data_version is per-connection, so db must be pinned to a single pooled
// connection (SetMaxOpenConns(1) — memory.OpenDB does this) for polls to
// compare against a stable baseline.
func Sync(ctx context.Context, ex *Exporter, db *sql.DB, vaultDir, projectFilter string, interval time.Duration) error {
	if err := ex.Export(ctx, vaultDir, projectFilter); err != nil {
		return err
	}
	last, err := dataVersion(ctx, db)
	if err != nil {
		return err
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			v, err := dataVersion(ctx, db)
			if err != nil {
				ex.Logger.Warn("obsidian sync: data_version poll failed", "error", err)
				continue
			}
			if v == last {
				continue
			}
			last = v
			if err := ex.Export(ctx, vaultDir, projectFilter); err != nil {
				ex.Logger.Warn("obsidian sync: export failed, will retry next change", "error", err)
			}
		}
	}
}

// dataVersion reads SQLite's per-connection change counter; it increments
// whenever another connection commits to the database.
func dataVersion(ctx context.Context, db *sql.DB) (int64, error) {
	var v int64
	err := db.QueryRowContext(ctx, "PRAGMA data_version").Scan(&v)
	return v, err
}
