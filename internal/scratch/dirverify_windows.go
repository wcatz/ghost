//go:build windows

package scratch

import (
	"errors"
	"os"
)

// errNotADir is returned when a path is not the real directory an earlier
// lstat described — a symlink or a non-directory.
var errNotADir = errors.New("path is not the directory first lstat'ed")

// sameRealDir reports whether path is still the directory want describes: a
// real directory (never a symlink) referring to the same file. Windows
// FileInfo has no portable device/inode identity, so the lstat checks are the
// whole of the defence; the path-based fallback remains a best-effort pass on
// a Ghost-owned root.
func sameRealDir(path string, want os.FileInfo) error {
	got, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if got.Mode()&os.ModeSymlink != 0 || !got.IsDir() {
		return errNotADir
	}
	if !os.SameFile(want, got) {
		return errNotADir
	}
	return nil
}
