//go:build !windows

package memory

import (
	"os"
	"syscall"
)

// lockFileExclusive blocks until it holds an exclusive advisory lock on f. It
// is flock, never fcntl: a flock belongs to the open file description, so two
// handles on the lock file inside ONE process exclude each other as two
// processes do, where a POSIX record lock is per process and would let them
// both through. The kernel drops the lock when the holder dies.
//
// The same pair as internal/mcpinit's lockExclusive/unlockFile, copied rather
// than imported: mcpinit imports this package.
func lockFileExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

// unlockFileLock releases a lock taken by lockFileExclusive.
func unlockFileLock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
