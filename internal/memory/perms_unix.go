//go:build !windows

package memory

import (
	"log/slog"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/wcatz/ghost/internal/config"
)

const permsGroupOther os.FileMode = 0o077

// TightenPermissions removes group and other access from the data directory and database files.
func TightenPermissions(dbPath string) {
	if dbPath == ":memory:" {
		return
	}
	if dir := filepath.Dir(dbPath); isDataDir(dir) {
		if info, err := os.Lstat(dir); err == nil && info.IsDir() {
			chmodTighten(dir, info)
		}
	}
	for i, name := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		info, err := os.Lstat(name)
		if err != nil {
			continue
		}
		if !info.Mode().IsRegular() {
			if i == 0 {
				slog.Warn("ghost database is not a regular file, so its permissions were left alone",
					"path", name, "mode", info.Mode())
			}
			continue
		}
		if i == 0 {
			chmodTighten(name, info)
		} else {
			chmodByName(name, info)
		}
	}
}

func isDataDir(dir string) bool {
	want, err := config.DataDirPath()
	if err != nil {
		return false
	}
	return dir == want
}

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
