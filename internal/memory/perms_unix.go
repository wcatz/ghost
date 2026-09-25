//go:build !windows

package memory

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/wcatz/ghost/internal/config"
)

// permsGroupOther is every group and other permission bit in a Unix mode. A
// Ghost data directory is meant to be 0700 and its database 0600, and
// stripping exactly these is what produces those: 0750 becomes 0700, 0640
// becomes 0600, and a WAL or shm file SQLite created at 0644 becomes 0600.
//
// config.DataDir already asks MkdirAll for 0700 and writes config.yaml at 0600,
// but neither is retroactive. MkdirAll leaves an existing directory's mode
// exactly as it found it, and no writer ever named a mode for the database, so
// an install created under a group-shared umask sat at 0750 with a 0640
// ghost.db — and the -wal and -shm files SQLite maintains beside it inherited
// the same width.
const permsGroupOther os.FileMode = 0o077

// TightenPermissions removes group and other access from the configured data
// directory and from the database's own three files. It never fails: a
// permission that cannot be tightened (a read-only or foreign-owned mount, an
// exotic filesystem) is logged and left alone, because refusing to go on over a
// mode bit would be the worse outcome and the caller has no way to recover.
//
// Exported because the tree has more than one read-write open. OpenDB is the
// only one that creates the database or migrates it, and calls this itself;
// mcpinit's bumpSessionCount is the session hook's one deliberate write, over
// its own short-lived rwDSN connection, and calls it too. Every other open in
// the tree is read-only and must stay that way — a diagnostic has to be able to
// report on a database it must not modify, and a read-only connection cannot
// create one. A mode that drifts is therefore repaired the first time Ghost
// writes, whichever of the two paths that is.
func TightenPermissions(dbPath string) {
	// An in-memory database has no directory and no files.
	if dbPath == ":memory:" {
		return
	}
	// Only the configured DataDir. dbPath may name a database in an eval
	// scratch tree, a test temp dir, or anywhere a user's environment pointed
	// at, and the directory holding it is not Ghost's to chmod.
	if dir := filepath.Dir(dbPath); isDataDir(dir) {
		if info, err := os.Lstat(dir); err == nil && info.IsDir() {
			chmodTighten(dir, info)
		}
	}
	// The database and the two files SQLite maintains beside it. A database
	// that has been checkpointed and closed has no -wal or -shm yet, so each of
	// those is optional here.
	for _, name := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		// An Lstat, and a regular-file test on the result: a symlink planted as
		// ghost.db is skipped rather than having its target chmod'ed through the
		// link, and a directory or socket where a file was expected is not
		// Ghost's to change either. A path that is not there is not an error —
		// it is the ordinary state of an unopened -wal.
		if info, err := os.Lstat(name); err == nil && info.Mode().IsRegular() {
			chmodTighten(name, info)
		}
	}
}

// isDataDir reports whether dir is the data directory config resolved, which
// is the one directory Ghost creates and therefore the one it may tighten. A
// DataDir that cannot be resolved at all matches nothing: this runs on the open
// path of a store that config has already located successfully, so a failure
// here means the two disagree, and the safe reading of that is to change
// nothing.
func isDataDir(dir string) bool {
	want, err := config.DataDirPath()
	if err != nil {
		return false
	}
	return dir == want
}

// chmodTighten removes the group and other permission bits from path and does
// nothing else. Being subtractive is the whole point: the pass can only narrow,
// so it can never widen a mode a user deliberately chose. A database locked to
// 0400 stays 0400 rather than being handed back 0600, an execute-only file
// keeps its execute bit, and a path already at 0700 or 0600 is never chmod'ed at
// all. info must come from an Lstat of path, and the caller has already
// established that path is of the kind Ghost owns.
func chmodTighten(path string, info os.FileInfo) {
	perm := info.Mode().Perm()
	tightened := perm &^ permsGroupOther
	if tightened == perm {
		return
	}
	if err := os.Chmod(path, tightened); err != nil {
		slog.Warn("could not tighten ghost data permissions",
			"path", path, "was", perm, "now", tightened, "error", err)
	}
}
