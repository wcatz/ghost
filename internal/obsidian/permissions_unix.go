//go:build !windows

package obsidian

import (
	"errors"
	"os"
	"syscall"
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
