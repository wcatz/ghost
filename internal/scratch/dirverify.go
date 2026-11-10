package scratch

import (
	"errors"
	"os"
)

// errNotADir is returned when a path is not the real directory an earlier
// lstat described — a symlink, a non-directory, or a different file.
var errNotADir = errors.New("path is not the directory first lstat'ed")

// sameRealDir reports whether path is still the directory want describes: a
// real directory (never a symlink) referring to the same file. A nil result
// means the path is unchanged and safe to call path-based callbacks on; any
// error means the path was swapped or renamed and must be abandoned.
func sameRealDir(path string, want os.FileInfo) error {
	got, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if got.Mode()&os.ModeSymlink != 0 || !got.IsDir() {
		return errNotADir
	}
	// os.SameFile compares the portable file identity (device and inode) the
	// platform's FileInfo reports, so a renamed directory — a different inode
	// at the same path — is refused.
	if !os.SameFile(want, got) {
		return errNotADir
	}
	return nil
}
