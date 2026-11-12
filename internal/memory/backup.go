package memory

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

const backupTimeLayout = "20060102T150405Z"

// BackupFileName returns the default destination for a backup of dbPath taken
// at the given instant: the database path with a ".backup-<UTC timestamp>"
func BackupFileName(dbPath string, at time.Time) string {
	return fmt.Sprintf("%s.backup-%s", dbPath, at.UTC().Format(backupTimeLayout))
}

// BackupCounts is the per-table row count of a backup file, in a fixed order so
// the printed report is stable. It counts what the snapshot holds, not what the
// live database held a moment later: a restore is checked against the file, so
// these are the numbers that have to match it.
type BackupCounts struct {
	Projects    int `json:"projects"`
	Memories    int `json:"memories"`
	MemoryLinks int `json:"memory_links"`
	Tasks       int `json:"tasks"`
	Decisions   int `json:"decisions"`
}

// BackupResult is what one backup wrote: where it went, how big the file is,
// what it contains, and where the manifest describing all three landed.
type BackupResult struct {
	Path         string
	Bytes        int64
	Counts       BackupCounts
	ManifestPath string
}

// Store.Backup writes a consistent snapshot of the live database to dest and
// reports what the snapshot holds.
func (s *Store) Backup(ctx context.Context, dest string) (BackupResult, error) {
	res, err := s.vacuumAndCount(ctx, dest)
	if err != nil {
		return BackupResult{}, err
	}
	manifestPath, err := writeBackupManifest(dest, res.Counts, time.Now())
	if err != nil {
		return BackupResult{}, fmt.Errorf("backup manifest: %w — the snapshot at %s is complete and restorable, but uncheckable until one can be written", err, dest)
	}
	res.ManifestPath = manifestPath
	return res, nil
}

// vacuumAndCount takes the copy and counts it, and returns everything but the
// manifest path. Split out of Backup so the extent of the lock is one function
// rather than a span a reader has to infer from where a defer happens to sit:
// the manifest write reads the whole file again, and must not be inside it (see
// Backup).
func (s *Store) vacuumAndCount(ctx context.Context, dest string) (BackupResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := vacuumInto(ctx, s.db, dest); err != nil {
		return BackupResult{}, err
	}
	info, err := os.Stat(dest)
	if err != nil {
		return BackupResult{}, afterVacuumFailure(dest, fmt.Errorf("stat backup: %w", err))
	}
	counts, err := countBackupContents(ctx, dest)
	if err != nil {
		return BackupResult{}, afterVacuumFailure(dest, err)
	}
	return BackupResult{Path: dest, Bytes: info.Size(), Counts: counts}, nil
}

// afterVacuumFailure is the account of a backup that failed after its vacuum
// returned. The CLI prints no report for a failed backup, so this error is the
// only description of what is now on disk at dest.
func afterVacuumFailure(dest string, cause error) error {
	if info, err := os.Lstat(dest); err == nil && info.Mode().IsRegular() && info.Size() == 0 {
		_ = os.Remove(dest)
		return cause
	}
	return fmt.Errorf("%w — a copy of the database was written to %s and is left in place; check it with ghost backup verify, or delete it", cause, dest)
}

func countBackupContents(ctx context.Context, path string) (BackupCounts, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return BackupCounts{}, fmt.Errorf("resolve backup path %s: %w", path, err)
	}
	db, err := OpenReadDB(abs)
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
// create the destination at 0600 before SQLite writes a byte of it, VACUUM INTO
// it, and run the permission pass afterwards.
func vacuumInto(ctx context.Context, db *sql.DB, dest string) error {
	target, err := vacuumTarget(dest)
	if err != nil {
		return err
	}
	if err := reserveBackupPath(dest); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, target); err != nil {
		_ = os.Remove(dest)
		return fmt.Errorf("vacuum into %s: %w", dest, err)
	}
	// window — the file is never born wider — so on a filesystem that honours the
	TightenPermissions(dest)
	return nil
}

func vacuumTarget(dest string) (string, error) {
	abs, err := filepath.Abs(dest)
	if err != nil {
		return "", fmt.Errorf("resolve backup path %s: %w", dest, err)
	}
	u := url.URL{Scheme: "file", Opaque: (&url.URL{Path: abs}).EscapedPath()}
	return u.String(), nil
}

// reserveBackupPath creates dest as an empty 0600 regular file and closes it, so
// the copy is never born wider than the database it came from and the path is
// claimed atomically.
func reserveBackupPath(dest string) error {
	if dir := filepath.Dir(dest); dir != "" {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			return fmt.Errorf("backup directory %s does not exist — create it, or pass a path inside one that does", dir)
		}
	}

	if info, err := os.Lstat(dest); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("refusing to write to %s, which is not a regular file", dest)
		}
		return fmt.Errorf("refusing to overwrite an existing file: %s", dest)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("check backup path %s: %w", dest, err)
	}

	f, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("refusing to overwrite an existing file: %s", dest)
		}
		return fmt.Errorf("create backup %s: %w", dest, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(dest)
		return fmt.Errorf("create backup %s: %w", dest, err)
	}
	return nil
}
