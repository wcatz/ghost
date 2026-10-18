//go:build windows

package memory

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockFileExclusive blocks until it holds an exclusive lock on the first byte
// of f. A LockFileEx lock belongs to the handle, so two handles in one process
// exclude each other as two processes do, and the OS drops it when the holder
// dies.
//
// The same pair as internal/mcpinit's lockExclusive/unlockFile, copied rather
// than imported: mcpinit imports this package.
func lockFileExclusive(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, ol)
}

// unlockFileLock releases a lock taken by lockFileExclusive.
func unlockFileLock(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, ol)
}
