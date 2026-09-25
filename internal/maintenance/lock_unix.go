//go:build !windows

package maintenance

import (
	"errors"
	"os"
	"syscall"
)

func tryLockExclusive(file *os.File) (bool, error) {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return false, nil
	}
	return false, err
}

func unlockProcessLock(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}

func removeOwnedLock(path string, _ *os.File) error {
	// Unlinking while the descriptor is locked is safe on Unix: no other
	// process can acquire this inode once the stale owner has released it.
	return os.Remove(path)
}
