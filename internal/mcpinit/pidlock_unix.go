//go:build !windows

package mcpinit

import (
	"os"
	"syscall"
)

// lockExclusive blocks until it holds an exclusive advisory lock on f.
// Unlike a plain claim file, this lock is held by the kernel and is released
// automatically if the process dies while holding it, so a crash between
// acquiring the lock and releasing it can never deadlock later claimants.
func lockExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

// unlockFile releases a lock taken by lockExclusive.
func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
