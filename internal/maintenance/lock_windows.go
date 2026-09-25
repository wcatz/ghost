//go:build windows

package maintenance

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func tryLockExclusive(file *os.File) (bool, error) {
	overlapped := new(windows.Overlapped)
	err := windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, overlapped,
	)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return false, err
}

func unlockProcessLock(file *os.File) error {
	overlapped := new(windows.Overlapped)
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, overlapped)
}

func removeOwnedLock(path string, file *os.File) error {
	// Windows does not permit removing an open handle. The lock is held by
	// this process and the path is already proven stale, so close it before
	// the best-effort unlink; a concurrent claimant can only recreate the lock
	// after this close, never inherit the stale inode.
	_ = file.Close()
	return os.Remove(path)
}
