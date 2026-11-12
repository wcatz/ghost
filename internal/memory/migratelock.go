package memory

import (
	"fmt"
	"os"
)

func migrationLockPath(dbPath string) string { return dbPath + ".migrate.lock" }

// acquireMigrationLock blocks until this caller holds the migration lock for
// dbPath, and returns the function that releases it.
func acquireMigrationLock(dbPath string) (func(), error) {
	if dbPath == ":memory:" {
		return func() {}, nil
	}
	path := migrationLockPath(dbPath)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open migration lock %s: %w", path, err)
	}
	if err := lockFileExclusive(f); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("take migration lock %s: %w", path, err)
	}
	return func() {
		_ = unlockFileLock(f)
		_ = f.Close()
	}, nil
}

func refuseNewerSchema(version int) error {
	if version > schemaVersion {
		return fmt.Errorf("database schema v%d is newer than this ghost build (v%d) — upgrade ghost before opening it", version, schemaVersion)
	}
	return nil
}
