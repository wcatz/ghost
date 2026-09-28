package memory

import (
	"context"
	"io"
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
//
// The JSON tags are the manifest's field names. They are here, on the one type
// both the printed report and the manifest use, because a second definition of
// the same five numbers would be a second place for them to drift — and a
// manifest reporting a different set of tables than the report prints beside it
// would be a manifest describing a file no reader can check.
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
	Path   string
	Bytes  int64
	Counts BackupCounts
	// ManifestPath is the sidecar written beside Path. It is reported rather
	// than left to convention because the printed report is the only place a
	// user learns the file exists, and a backup whose description is in a file
	// nobody was told about is a backup nobody will check.
	ManifestPath string
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
// The write lock is held for the length of the vacuum and the count that follows
// it, and no longer. It is not wrapped in a transaction, and no other lock is
// taken on the way, so a running MCP server keeps serving for the length of one
// vacuum and not for a copy-then-check sequence — the manifest write below is
// deliberately OUTSIDE it, for the same reason.
//
// dest must not exist. Both this and the pre-migration copy refuse rather than
// replace, because the file already there is the previous backup someone may
// still be relying on. An Lstat is used so a dangling symlink at that path is
// refused too, rather than being written through to whatever it points at.
//
// A manifest lands beside the snapshot (see writeBackupManifest), and a failure
// to write it fails the backup rather than being reported as a success with a
// quiet omission: the whole point of a copy is that it can be checked later, and
// a snapshot nothing can check is a claim, not a backup. The snapshot itself is
// left in place when that happens — it is a real, restorable copy, and deleting
// a user's only backup because a sidecar could not be written would trade a
// smaller problem for a larger one.
//
// The manifest is written AFTER s.mu is released, and that is load-bearing rather
// than incidental. Hashing it re-reads every byte of the snapshot, and a second
// full pass over the database's bytes inside this store's EXCLUSIVE lock would
// stall every reader — every ghost_memory_search through a live server — for a
// read of a file that belongs to nobody but this call. By the time the lock is
// dropped the snapshot is complete and closed and O_EXCL has claimed its path,
// so nothing else can be writing it and the manifest can be built from the path
// alone, which is all it takes.
func (s *Store) Backup(ctx context.Context, dest string) (BackupResult, error) {
	res, err := s.vacuumAndCount(ctx, dest)
	if err != nil {
		return BackupResult{}, err
	}
	manifestPath, err := writeBackupManifest(dest, res.Counts, time.Now())
	if err != nil {
		return BackupResult{}, fmt.Errorf("backup manifest: %w", err)
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

	if err := fileCopySnapshot(ctx, s.db, dest); err != nil {
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
	db, err := OpenReadDB(path)
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
//
// The pre-creation is the whole point of this function and it exists for two
// reasons that a create-then-chmod cannot serve.
//
// The width. VACUUM INTO names no mode for the file it creates, so the copy
// lands at SQLite's default minus the umask — measured at 0644 with a umask of
// 000. It is only shielded by the 0700 data directory, and a --out destination
// outside it has no such shield, so chmod'ing afterwards leaves a window in which
// a full copy of the memory database is group- and world-readable. Creating the
// file at 0600 first means it is never born wider. SQLite accepts an existing
// EMPTY file as a VACUUM INTO destination and keeps its mode (verified against
// v1.x through modernc.org/sqlite); it refuses a non-empty one, which is what
// makes the emptiness this function guarantees into the thing SQLite checks.
//
// The reservation. O_EXCL is the atomic "this path is mine" test, so it replaces
// a stat-then-create pair: there is no window between "I looked and it was
// absent" and "I created it" for another writer, or a symlink swap, to slip into.
// It also refuses a dangling symlink at the path, which a stat-based check reads
// as absent and which the write would otherwise go through to its target.
//
// A vacuum that fails releases the reservation, so a retry is not blocked by
// this run's own empty leftover. A process killed between the create and the
// vacuum does leave one, and the next run reports it as an existing file — the
// empty file is 0600 and contains nothing, so deleting it is safe.
func vacuumInto(ctx context.Context, db *sql.DB, dest string) error {
	if err := reserveBackupPath(dest); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, dest); err != nil {
		_ = os.Remove(dest)
		return fmt.Errorf("vacuum into %s: %w", dest, err)
	}
	// Second line, not the only one. The reservation above is what closes the
	// window — the file is never born wider — so on a filesystem that honours the
	// open mode this call has nothing left to do, and a mutation that removes it
	// survives the test suite. It is kept because the open mode is advisory:
	// setgid directories, ACLs and some network and FUSE mounts can leave a file
	// wider than the mode asked for, and this is a full copy of the memory
	// database. Two cheap checks, neither load-bearing alone.
	TightenPermissions(dest)
	return nil
}

// fileCopySnapshot stands in for VACUUM INTO in this mutation only.
func fileCopySnapshot(ctx context.Context, db *sql.DB, dest string) error {
	var seq, name, path string
	if err := db.QueryRowContext(ctx, "PRAGMA database_list").Scan(&seq, &name, &path); err != nil {
		return err
	}
	in, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	f, err := os.OpenFile(dest, os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(f, in)
	return err
}

// reserveBackupPath creates dest as an empty 0600 regular file and closes it, so
// the copy is never born wider than the database it came from and the path is
// claimed atomically.
//
// It is its own function because these are the two properties no caller can check
// after the fact: once a backup has finished, a create-then-chmod and a
// create-at-0600 are indistinguishable — TightenPermissions ends up at the same
// mode either way — and the window in between is the whole reason this exists.
//
// The handle is closed before returning. SQLite opens the path itself for the
// vacuum, and holding a descriptor across it would be a second writer on a file
// this is only reserving.
func reserveBackupPath(dest string) error {
	// The parent directory first. A --out destination is usually typed by hand,
	// and a missing directory is the likely mistake; SQLite reports it as "unable
	// to open database file", which reads as a problem with the database rather
	// than with the path the user just typed. It is here rather than in
	// vacuumInto because this is where a destination is validated.
	if dir := filepath.Dir(dest); dir != "" {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			return fmt.Errorf("backup directory %s does not exist — create it, or pass a path inside one that does", dir)
		}
	}

	// Classified before the create, so the refusal says which of the two cases it
	// is: O_EXCL reports both as EEXIST, but "that file is already there" and
	// "that path is a symlink and I will not write through it" are different
	// things to be told. The create below is what enforces both — this
	// classifies, it does not decide.
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
			// The window between the Lstat above and this create. O_EXCL is what
			// closes it, and this is only the report of who won.
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
