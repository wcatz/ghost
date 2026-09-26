package memory

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// backupTimeLayout is the timestamp suffix a default backup name carries:
// UTC, sortable, seconds wide, and using no character that is illegal in a
// Windows path (a colon in "15:32:07" would be). It is the same layout the
// pre-migrate copy's name uses, so one convention covers both.
const backupTimeLayout = "20060102T150405Z"

// BackupFileName returns the default destination for a backup of dbPath taken
// at the given instant: the database path with a ".backup-<UTC timestamp>"
// suffix, so the copy lands beside the live database. The instant is
// converted to UTC rather than formatted as given — a backup name has to say
// the same thing on every machine that reads it.
func BackupFileName(dbPath string, at time.Time) string {
	return fmt.Sprintf("%s.backup-%s", dbPath, at.UTC().Format(backupTimeLayout))
}

// BackupCounts is the per-table row count of a backup file, in a fixed order so
// the printed report is stable. It counts what the snapshot holds, not what the
// live database held a moment later: a restore is checked against the file, so
// these are the numbers that have to match it.
type BackupCounts struct {
	Projects    int
	Memories    int
	MemoryLinks int
	Tasks       int
	Decisions   int
}

// BackupResult is what one backup wrote: where it went, how big the file is,
// and what it contains.
type BackupResult struct {
	Path   string
	Bytes  int64
	Counts BackupCounts
}

// Store.Backup writes a consistent snapshot of the live database to dest and
// reports what the snapshot holds.
//
// It uses SQLite's VACUUM INTO, which is a read of the source rather than a
// copy of its files: the destination is built from a single read snapshot, so a
// writer committing during the backup is reflected in it whole or not at all.
// That is the property a hand-rolled `cp` of a WAL database cannot offer, where
// copying ghost.db without a matching -wal loses whatever the log held, and
// copying the pair while a write lands can capture a torn page.
//
// The write lock is held for the duration of the VACUUM and nothing longer:
// this is not wrapped in a transaction, and no other lock is taken on the way,
// so a running MCP server keeps serving for the length of one vacuum and not
// for a copy-then-check sequence.
//
// dest must not exist. Both this and the pre-migration copy refuse rather than
// replace, because the file already there is the previous backup someone may
// still be relying on. An Lstat is used so a dangling symlink at that path is
// refused too, rather than being written through to whatever it points at.
func (s *Store) Backup(ctx context.Context, dest string) (BackupResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := vacuumInto(ctx, s.db, dest); err != nil {
		return BackupResult{}, err
	}
	info, err := os.Stat(dest)
	if err != nil {
		return BackupResult{}, fmt.Errorf("stat backup: %w", err)
	}
	// Counted from the snapshot, not from s.db: the counts a restore is checked
	// against have to describe the file that was written, and a writer that
	// committed after the VACUUM would otherwise make the two disagree.
	counts, err := countBackupContents(ctx, dest)
	if err != nil {
		return BackupResult{}, err
	}
	return BackupResult{Path: dest, Bytes: info.Size(), Counts: counts}, nil
}

// countBackupContents counts the rows a restore cares about in a written
// snapshot. It opens the file read-only and runs no DDL, so counting a backup
// cannot change it and cannot create a database where the backup failed to
// land.
func countBackupContents(ctx context.Context, path string) (BackupCounts, error) {
	db, err := OpenDBReadOnly(path)
	if err != nil {
		return BackupCounts{}, fmt.Errorf("open backup for counting: %w", err)
	}
	defer db.Close() //nolint:errcheck
	return CountRows(ctx, db)
}

// CountRows counts the rows a backup report prints, in the order the report
// prints them. Exported so a caller holding only a *sql.DB — the pre-migration
// path, which has no Store yet — can produce the same numbers.
func CountRows(ctx context.Context, db *sql.DB) (BackupCounts, error) {
	var counts BackupCounts
	queries := []struct {
		to   *int
		sql  string
		name string
	}{
		{&counts.Projects, `SELECT count(*) FROM projects`, "projects"},
		{&counts.Memories, `SELECT count(*) FROM memories`, "memories"},
		{&counts.MemoryLinks, `SELECT count(*) FROM memory_links`, "memory_links"},
		{&counts.Tasks, `SELECT count(*) FROM tasks`, "tasks"},
		{&counts.Decisions, `SELECT count(*) FROM decisions`, "decisions"},
	}
	for _, q := range queries {
		if err := db.QueryRowContext(ctx, q.sql).Scan(q.to); err != nil {
			return BackupCounts{}, fmt.Errorf("count %s: %w", q.name, err)
		}
	}
	return counts, nil
}

// vacuumInto is the one implementation of "copy this database to that path":
// VACUUM INTO, a refusal to replace an existing file, and the permission pass
// that narrows the copy to the width of the database it came from.
//
// VACUUM INTO names no mode for the file it creates, so the copy lands at
// whatever SQLite's default is minus the umask — a full copy of the memory
// database at a width wider than the live one. It is only shielded by the 0700
// data directory, and a --out destination outside it has no such shield, so the
// copy is tightened here rather than left to a pass that walks only the three
// live files. TightenPermissions never widens a mode, so a destination the user
// had already made narrower keeps its own.
func vacuumInto(ctx context.Context, db *sql.DB, dest string) error {
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("refusing to overwrite an existing file: %s", dest)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("check backup path %s: %w", dest, err)
	}
	// A --out destination is usually typed by hand, and a missing parent
	// directory is the likely mistake. SQLite reports it as "unable to open
	// database file", which reads as a problem with the database rather than
	// with the path, so the directory is checked here and named.
	if dir := filepath.Dir(dest); dir != "" {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			return fmt.Errorf("backup directory %s does not exist — create it, or pass a path inside one that does", dir)
		}
	}
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, dest); err != nil {
		return fmt.Errorf("vacuum into %s: %w", dest, err)
	}
	TightenPermissions(dest)
	return nil
}
