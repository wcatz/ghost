package memory

import (
	"fmt"
	"os"
)

// migrationLockPath is the file whose exclusive lock serialises the
// decide-backup-migrate sequence of OpenDB across every process and handle that
// opens dbPath. It sits beside the database, so it is in whatever directory the
// caller already resolved (and GHOST_DEV_FORBID_DATA_DIR already vetted), and
// its name cannot match prunePreMigrateBackups' "<db>.pre-migrate-<unix>".
func migrationLockPath(dbPath string) string { return dbPath + ".migrate.lock" }

// acquireMigrationLock blocks until this caller holds the migration lock for
// dbPath, and returns the function that releases it.
//
// SQLite cannot be the lock here, which is why a file is: the sequence it
// guards spans a VACUUM INTO (the pre-migration copy), which SQLite refuses to
// run inside a transaction, and then one transaction per migration step with
// PRAGMA foreign_keys toggled between them, which is a no-op inside one. No
// single write transaction can hold the write lock across all of it, and a lock
// released between the copy and the steps is a window for a second opener to
// copy and migrate too.
//
// The wait is unbounded on purpose. The holder is copying and migrating the
// whole store, which on a large one legitimately takes longer than any timeout
// worth picking, and a holder that dies releases the lock with it (flock and
// LockFileEx are both dropped by the OS), so a crash cannot strand the waiters.
//
// The file is never removed. Unlinking a lock file while another opener is
// blocked on it hands that opener a lock on an inode nobody else will ever
// open again, so the next opener locks a new file and both run at once; and
// Windows cannot delete a file another handle has open.
func acquireMigrationLock(dbPath string) (func(), error) {
	if dbPath == ":memory:" {
		// A private in-memory database has one handle, and nowhere to put a file.
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

// refuseNewerSchema is OpenDB's refusal of a store a newer ghost wrote. It is
// one function because OpenDB asks twice — before any DDL, and again under the
// migration lock, where a newer build may have migrated the store meanwhile —
// and the two answers must be the same answer.
func refuseNewerSchema(version int) error {
	if version > schemaVersion {
		return fmt.Errorf("database schema v%d is newer than this ghost build (v%d) — upgrade ghost before opening it", version, schemaVersion)
	}
	return nil
}
