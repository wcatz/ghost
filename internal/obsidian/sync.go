package obsidian

import (
	"context"
	"database/sql"
	"time"
)

// Sync re-exports whenever the database changes, polling PRAGMA data_version.
func Sync(ctx context.Context, ex *Exporter, db *sql.DB, vaultDir, projectFilter string, interval time.Duration) error {
	return ex.Export(ctx, vaultDir, projectFilter) // poll loop lands in the next commit
}
