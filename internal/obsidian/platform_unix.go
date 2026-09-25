//go:build !windows

package obsidian

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// protectPath restricts a path to its owner. On POSIX that is the mode:
// compare against what the walk already stat'ed, so an already-tight vault
// costs no syscall beyond the walk itself and an inherited 0755/0644 vault
// is corrected on the next ensureVault.
func protectPath(path string, info os.FileInfo, want os.FileMode) error {
	if info.Mode().Perm() == want {
		return nil
	}
	return os.Chmod(path, want)
}

// protectFile secures a file Ghost is about to publish, through its open
// handle: the name could be swapped between stat and rename, and the
// descriptor pins the inode that is actually written.
func protectFile(f *os.File) error {
	return f.Chmod(0o600)
}

// isDirNotEmpty reports whether os.Remove failed only because the directory
// still holds entries — the guard doing its job — as opposed to a
// permission, sharing, or I/O failure that prune must surface.
func isDirNotEmpty(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY)
}

// makeFIFO creates a named pipe for the special-file prune regression. Not
// a test helper alone: prune's regular-file guard is the behaviour under
// test, and the walk reaches the FIFO through the same code on every OS.
func makeFIFO(path string) error {
	return syscall.Mkfifo(path, 0o600)
}

// isTransientRenameErr reports whether a failed rename is worth retrying.
// POSIX rename either lands or fails for a reason a retry cannot change (a
// missing path, a cross-device link), so nothing is transient here — unlike
// Windows, where the replace can lose a race with any reader.
func isTransientRenameErr(err error) bool {
	return false
}

// renameNoReplace moves oldPath to newPath only if newPath does not exist.
//
// Go's os.Rename replaces an existing destination unconditionally, so a
// restore built on it silently destroys whatever appeared at the path in the
// window between the existence check and the rename. There is no no-replace
// rename in the standard library on POSIX, but link(2) has the property
// restore needs: it fails with EEXIST rather than replacing, and it is atomic
// with respect to the destination. Unlinking the old name afterwards is safe
// because the object is already published under its original name by then.
func renameNoReplace(oldPath, newPath string) error {
	if err := os.Link(oldPath, newPath); err != nil {
		return err
	}
	if err := os.Remove(oldPath); err != nil {
		// The object is now published under both names. Drop the new name so a
		// half-applied restore leaves no duplicate, and report the failure.
		_ = os.Remove(newPath)
		return err
	}
	return nil
}

// openRegular opens path for reading and returns the handle only if the object
// it refers to is a regular file.
//
// The name is never followed as a symlink and the open cannot block on a FIFO,
// so the file type is decided by fstat on the handle that is actually read
// rather than by a directory entry stat'ed earlier. That closes a TOCTOU the
// regular-file guard otherwise leaves open: a note classified as a regular file
// and then swapped for a FIFO before the open would hang the caller forever
// (O_NONBLOCK stops the open; O_NOFOLLOW refuses the symlink), which is exactly
// the export hang the guard exists to prevent.
func openRegular(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, &os.PathError{Op: "open", Path: path, Err: syscall.ENOTDIR}
	}
	return f, nil
}
