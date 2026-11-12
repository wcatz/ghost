package obsidian

import (
	"context"
	"database/sql"
	"time"
)

// Sync mirrors the store to the vault until ctx is cancelled.
func Sync(ctx context.Context, ex *Exporter, db *sql.DB, vaultDir, projectFilter string, interval time.Duration) error {
	last, err := dataVersion(ctx, db)
	if err != nil {
		return err
	}
	if err := ex.Export(ctx, vaultDir, projectFilter); err != nil {
		ex.Logger.Warn("obsidian sync: initial export failed, will retry next tick", "error", err)
		last = -1
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
			if err := ex.Export(ctx, vaultDir, projectFilter); err != nil {
				ex.Logger.Warn("obsidian sync: export failed, will retry next tick", "error", err)
				continue
			}
			last = v
		}
	}
}

func dataVersion(ctx context.Context, db *sql.DB) (int64, error) {
	var v int64
	err := db.QueryRowContext(ctx, "PRAGMA data_version").Scan(&v)
	return v, err
}
