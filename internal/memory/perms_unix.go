//go:build !windows

package memory

import (
	"log/slog"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

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
// a data directory something else created first kept its 0750, and the
// database — which SQLite creates at its own default minus the umask — sat at
// 0640 beside it, with the -wal and -shm files inheriting the same width.
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
	for i, name := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		info, err := os.Lstat(name)
		if err != nil {
			// A path that is not there is not an error — it is the ordinary
			// state of an unopened -wal. Anything else is reported by the open
			// itself.
			continue
		}
		if !info.Mode().IsRegular() {
			// A symlink planted as ghost.db is skipped rather than having its
			// target chmod'ed through the link, and a directory or socket where
			// a file was expected is not Ghost's to change either.
			//
			// The database itself gets a warning, because a symlinked ghost.db
			// is a legitimate setup (a synced data directory, a dotfiles
			// checkout) and the consequence is that this pass silently never
			// applies to the real database: the user would have no way to tell
			// an unprotected install from a protected one. The -wal and -shm are
			// silent, since they are transient and a warning on every open
			// would be noise.
			if i == 0 {
				slog.Warn("ghost database is not a regular file, so its permissions were left alone",
					"path", name, "mode", info.Mode())
			}
			continue
		}
		// The database through a descriptor, the two files SQLite maintains
		// beside it by name: see fchmodNoFollow for why the split is there.
		if i == 0 {
			chmodTighten(name, info)
		} else {
			chmodByName(name, info)
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
	if err := fchmodNoFollow(path, tightened); err != nil {
		slog.Warn("could not tighten ghost data permissions",
			"path", path, "was", perm, "now", tightened, "error", err)
	}
}

// fchmodNoFollow applies mode to the file path names, through a descriptor
// rather than through the name.
//
// os.Chmod resolves the name a second time, so the Lstat the caller took and
// the chmod that acted on it were two lookups of one path: anything that
// replaced a regular file with a symlink in between had the mode applied to the
// link's target. Inside the data directory that needs write access to a 0700
// directory, but dbPath also names a database in an eval scratch tree, a bench
// tree or a maintenance tree, where the parent is the user's own and nothing
// stops a rename. The consequence is not a wider mode — it is a NARROWER one
// applied to a file Ghost does not own, which is how a pass that promises "only
// ever subtractive" becomes destructive.
//
// O_NOFOLLOW makes the open itself the decision, so there is no second lookup
// to be wrong about, and File.Chmod is fchmod(2) on the descriptor the open
// returned. A symlink at the name is therefore refused (ELOOP) rather than
// followed, and a path that stopped being a regular file in the window is left
// alone — the pass's contract is to never make Ghost's own data less
// protected, and a refusal here is reported by the caller's warning.
//
// The cost is a descriptor, and one shape the old name-based call handled: a
// file with no read bit for its owner (0220 and the like) cannot be opened
// O_RDONLY, so it keeps its group and other bits and the caller logs a warning.
// A database Ghost can open at all needs to read it, so that mode is a
// curiosity rather than a case, and the alternative — a write-only fallback —
// would put the name back in the path and reopen the window this closes.
//
// It is used for the DATABASE only. The -wal and -shm files keep the
// name-based chmod, and that is not an oversight: OpenDB calls this pass from
// inside the open, before the connection is established, and opening either of
// them read-only at that point changes what a later write records — measured
// with the e2e suite, where a save after a delete stopped writing its history
// row at all once this ran on all three files. They are also the files the
// reasoning above does not reach: SQLite creates and replaces them itself, it
// will not use a symlinked one, and a swap there cannot outlive the database
// file the next open validates.
func fchmodNoFollow(path string, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// chmodByName is the name-based chmod TightenPermissions still uses for the
// database's -wal and -shm, kept separate from chmodTighten so the reason
// above sits on the choice rather than in a comment nobody reading the caller
// will find. Being subtractive, like the descriptor path: it can only narrow.
func chmodByName(path string, info os.FileInfo) {
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
