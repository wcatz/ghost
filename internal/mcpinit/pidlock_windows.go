//go:build windows

package mcpinit

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockExclusive blocks until it holds an exclusive advisory lock on f.
// Unlike a plain claim file, this lock is held by the OS and is released
// automatically if the process dies while holding it, so a crash between
// acquiring the lock and releasing it can never deadlock later claimants.
func lockExclusive(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, ol)
}

// unlockFile releases a lock taken by lockExclusive.
func unlockFile(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, ol)
}
