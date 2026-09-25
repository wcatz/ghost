// Package fileguard provides the leaf filesystem safety primitives shared by
// Ghost data-dir retention and Obsidian orphan-temp cleanup. It owns no
// Ghost policy: callers decide which paths are eligible.
package fileguard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	// ProbeTimeout bounds the complete external+native probe sequence.
	ProbeTimeout = 2 * time.Second
	// QuarantineGrace is how long an unheld tombstone must age before reaping.
	QuarantineGrace   = time.Hour
	quarantineDirName = ".ghost-quarantine"
)

// OpenFileProbe reports whether a path is held by a process.
type OpenFileProbe func(string) (bool, error)

var activeProbe OpenFileProbe = DetectOpenFile

// SetProbeForTest installs a deterministic process-wide probe and returns a
// restore function. It is a test seam inside an internal package.
func SetProbeForTest(probe OpenFileProbe) func() {
	old := activeProbe
	if probe == nil {
		activeProbe = DetectOpenFile
	} else {
		activeProbe = probe
	}
	return func() { activeProbe = old }
}

func currentProbe() OpenFileProbe {
	if activeProbe == nil {
		return DetectOpenFile
	}
	return activeProbe
}

// Lock is a held cross-process advisory lock.
type Lock struct{ file *os.File }

// Close releases the lock.
func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := unlockProcessLock(l.file)
	closeErr := l.file.Close()
	l.file = nil
	if err != nil {
		return err
	}
	return closeErr
}

// TryAcquireLock takes lockPath without blocking. The lock file is persistent.
func TryAcquireLock(lockPath string) (*Lock, bool, error) {
	file, err := openProcessLock(lockPath)
	if err != nil {
		return nil, false, err
	}
	locked, err := tryLockExclusive(file)
	if err != nil || !locked {
		_ = file.Close()
		return nil, locked, err
	}
	return &Lock{file: file}, true, nil
}

// AcquireLock takes lockPath, blocking until it is available. Callers must keep
// the critical section to filesystem-only work.
func AcquireLock(lockPath string) (*Lock, error) {
	file, err := openProcessLock(lockPath)
	if err != nil {
		return nil, err
	}
	if err := lockProcessLock(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &Lock{file: file}, nil
}

// QuarantineDir returns the private quarantine directory for path's parent.
func QuarantineDir(path string) (string, error) {
	dir := filepath.Join(filepath.Dir(path), quarantineDirName)
	if info, err := os.Lstat(dir); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("quarantine path is not a directory: %s", dir)
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// IsQuarantineDir reports whether path is a private quarantine directory.
func IsQuarantineDir(path string) bool {
	return filepath.Base(path) == quarantineDirName
}

// Quarantine probes path, atomically renames it into the private quarantine
// directory, and probes the moved inode again. The caller owns the returned
// tombstone and must Restore or RemoveQuarantine it.
func Quarantine(path string, probe OpenFileProbe) (string, error) {
	if probe == nil {
		probe = currentProbe()
	}
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", os.ErrNotExist
		}
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("refusing to quarantine non-regular file %s", path)
	}
	inUse, err := probe(path)
	if err != nil {
		return "", err
	}
	if inUse {
		return "", ErrHeld
	}
	dir, err := QuarantineDir(path)
	if err != nil {
		return "", err
	}
	placeholder, err := os.CreateTemp(dir, "tombstone-"+filepath.Base(path)+"-*")
	if err != nil {
		return "", err
	}
	tombstone := placeholder.Name()
	if err := placeholder.Close(); err != nil {
		_ = os.Remove(tombstone)
		return "", err
	}
	if err := os.Remove(tombstone); err != nil {
		return "", err
	}
	if err := os.Rename(path, tombstone); err != nil {
		if renameMeansHeld(err) {
			return "", ErrHeld
		}
		return "", err
	}
	// Rename preserves source mtime; quarantine age must start now.
	now := time.Now()
	_ = os.Chtimes(tombstone, now, now)
	inUse, err = probe(tombstone)
	if err != nil {
		return tombstone, errors.Join(err, Restore(path, tombstone))
	}
	if inUse {
		return tombstone, errors.Join(ErrHeld, Restore(path, tombstone))
	}
	return tombstone, nil
}

// ErrHeld means a candidate is held (or could not be renamed on Windows) and
// must be deferred without treating it as an operational failure.
var ErrHeld = errors.New("file is held")

// Restore moves tombstone back to path.
func Restore(path, tombstone string) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("cannot restore %s: replacement already exists", path)
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.Rename(tombstone, path)
}

// RemoveQuarantine removes a tombstone after a regular-file check.
func RemoveQuarantine(tombstone string) error {
	info, err := os.Lstat(tombstone)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("refusing to remove non-regular quarantined file %s", tombstone)
	}
	if err := os.Remove(tombstone); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// RemoveIfUnheld quarantines and removes path only when both probes prove it
// is unheld. Held, missing, and non-regular paths are deferred.
func RemoveIfUnheld(path string) (bool, error) {
	return RemoveIfUnheldWithProbe(path, currentProbe())
}

// RemoveIfUnheldWithProbe is RemoveIfUnheld with an explicit probe.
func RemoveIfUnheldWithProbe(path string, probe OpenFileProbe) (bool, error) {
	tombstone, err := Quarantine(path, probe)
	if errors.Is(err, ErrHeld) || os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := RemoveQuarantine(tombstone); err != nil {
		return false, err
	}
	return true, nil
}

// ReapStaleQuarantine removes old, unheld tombstones below root.
func ReapStaleQuarantine(root string) (int, error) {
	return ReapStaleQuarantineWithProbe(root, currentProbe())
}

// ReapStaleQuarantineWithProbe is ReapStaleQuarantine with an explicit probe.
func ReapStaleQuarantineWithProbe(root string, probe OpenFileProbe) (int, error) {
	return ReapQuarantineDirWithProbe(filepath.Join(root, quarantineDirName), probe)
}

// ReapQuarantineDir reaps old unheld tombstones from an already-resolved
// quarantine directory.
func ReapQuarantineDir(dir string) (int, error) {
	return ReapQuarantineDirWithProbe(dir, currentProbe())
}

// ReapQuarantineDirWithProbe is ReapQuarantineDir with an explicit probe.
func ReapQuarantineDirWithProbe(dir string, probe OpenFileProbe) (int, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return 0, fmt.Errorf("quarantine path is not a directory: %s", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
	var errs []error
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, statErr := os.Lstat(path)
		if statErr != nil || !info.Mode().IsRegular() || time.Since(info.ModTime()) < QuarantineGrace {
			continue
		}
		inUse, probeErr := probe(path)
		if probeErr != nil {
			errs = append(errs, probeErr)
			continue
		}
		if inUse {
			continue
		}
		if removeErr := RemoveQuarantine(path); removeErr != nil {
			errs = append(errs, removeErr)
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

// StageFile creates a fully writable staging file inside path's private
// quarantine directory. The caller must publish or remove it.
func StageFile(path string) (*os.File, error) {
	dir, err := QuarantineDir(path)
	if err != nil {
		return nil, err
	}
	return os.CreateTemp(dir, "stage-*")
}

// PublishNoReplace atomically exposes a fully written staging file at path
// without replacing a file a non-cooperating writer created in the meantime.
func PublishNoReplace(stage, path string) error {
	if err := os.Link(stage, path); err == nil {
		return os.Remove(stage)
	} else if os.IsExist(err) {
		return ErrPublishExists
	}
	// Some filesystems do not support hard links. With the shared log lock held,
	// a missing target is safe to publish by rename.
	if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
		if statErr != nil {
			return statErr
		}
		return ErrPublishExists
	}
	return os.Rename(stage, path)
}

// ErrPublishExists means a writer recreated the visible path during rotation.
var ErrPublishExists = errors.New("publish target already exists")

// RewriteQuarantine bounds a tombstone in place after a publish race.
func RewriteQuarantine(path string, tail []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	if _, err := file.Write(tail); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func openProcessLock(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("process lock is not a regular file: %s", path)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
}

// DetectOpenFile uses a bounded external probe, then a native fallback.
func DetectOpenFile(path string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ProbeTimeout)
	defer cancel()
	return detectOpenFile(ctx, path)
}
