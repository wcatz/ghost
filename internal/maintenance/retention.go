// Package maintenance owns bounded, best-effort cleanup of Ghost-owned files
// in the data directory. It deliberately operates on an explicit directory
// and database path: retention must never create a data directory as a side
// effect of a status, hook, or failed cleanup pass.
package maintenance

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/procstat"
)

var (
	// openFileProbe is a narrow seam for tests and for the production lsof/
	// fuser probe. A probe error is fail-closed: the candidate is left alone.
	openFileProbe = detectOpenFile

	knownLogNames = []string{
		"lifecycle.log",
		"obsidian-sync.log",
		"reflect.log",
		"resolve.log",
		"supersede.log",
	}
)

// Result is the count of files changed by one retention pass.
type Result struct {
	BackupsRemoved    int
	LogsRotated       int
	StaleFilesRemoved int
}

// Run loads the retention settings and performs one best-effort pass. Each
// component runs even if an earlier component reports an error; the combined
// error is for diagnostics and must not be treated as a failed Ghost open.
func Run(dataDir, dbPath string) (Result, error) {
	cfg, err := config.Load()
	if err != nil {
		return Result{}, fmt.Errorf("load retention config: %w", err)
	}
	return RunWithConfig(dataDir, dbPath, cfg.Retention.BackupCount, cfg.Retention.LogMaxBytes)
}

// RunWithConfig performs retention with already-loaded settings. Keeping the
// settings explicit lets callers that have a Config avoid a second config load
// while tests can pin the safety boundaries without touching global env state.
func RunWithConfig(dataDir, dbPath string, backupCount int, logMaxBytes int64) (Result, error) {
	var result Result
	var errs []error

	removed, err := PrunePreMigrateBackups(dbPath, backupCount)
	result.BackupsRemoved = removed
	if err != nil {
		errs = append(errs, fmt.Errorf("prune pre-migrate backups: %w", err))
	}
	rotated, err := RotateLogs(dataDir, logMaxBytes)
	result.LogsRotated = rotated
	if err != nil {
		errs = append(errs, fmt.Errorf("rotate data-dir logs: %w", err))
	}
	stale, err := ReapStaleProcessFiles(dataDir)
	result.StaleFilesRemoved = stale
	if err != nil {
		errs = append(errs, fmt.Errorf("reap stale process files: %w", err))
	}
	return result, errors.Join(errs...)
}

// backupCandidate is a regular, timestamped pre-migrate copy. The timestamp
// parsed from the filename breaks ties, while mtime reflects when a copy was
// actually created if the wall clock moved between migrations.
type backupCandidate struct {
	path  string
	name  string
	stamp int64
	mtime int64
}

// PrunePreMigrateBackups removes old copies beside dbPath, retaining the
// newest keep numeric-timestamped regular files. A non-positive keep is an
// explicit opt-out. The live DB is never a candidate, and os.SameFile also
// protects a hard link wearing the backup name.
func PrunePreMigrateBackups(dbPath string, keep int) (int, error) {
	if keep <= 0 || dbPath == "" || dbPath == ":memory:" {
		return 0, nil
	}
	dir := filepath.Dir(dbPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read data dir %s: %w", dir, err)
	}

	var liveInfo os.FileInfo
	if info, statErr := os.Lstat(dbPath); statErr == nil {
		liveInfo = info
	}
	prefix := filepath.Base(dbPath) + ".pre-migrate-"
	candidates := make([]backupCandidate, 0)
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		stamp, parseErr := strconv.ParseInt(strings.TrimPrefix(entry.Name(), prefix), 10, 64)
		if parseErr != nil {
			return 0, fmt.Errorf("unrecognized pre-migrate backup name %s", entry.Name())
		}
		path := filepath.Join(dir, entry.Name())
		info, statErr := os.Lstat(path)
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			continue
		}
		if liveInfo != nil && os.SameFile(liveInfo, info) {
			continue
		}
		candidates = append(candidates, backupCandidate{
			path:  path,
			name:  entry.Name(),
			stamp: stamp,
			mtime: info.ModTime().UnixNano(),
		})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].mtime != candidates[j].mtime {
			return candidates[i].mtime > candidates[j].mtime
		}
		if candidates[i].stamp != candidates[j].stamp {
			return candidates[i].stamp > candidates[j].stamp
		}
		return candidates[i].name > candidates[j].name
	})
	if len(candidates) <= keep {
		return 0, nil
	}

	removed := 0
	var errs []error
	for _, candidate := range candidates[keep:] {
		inUse, probeErr := openFileProbe(candidate.path)
		if probeErr != nil {
			// Cannot verify means cannot delete. Leave the file for a later pass.
			errs = append(errs, fmt.Errorf("probe %s: %w", candidate.path, probeErr))
			continue
		}
		if inUse {
			continue
		}
		if err := os.Remove(candidate.path); err != nil {
			if !os.IsNotExist(err) {
				errs = append(errs, fmt.Errorf("remove %s: %w", candidate.path, err))
			}
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

// RotateLogs bounds the known Ghost-owned data-dir logs without deleting an
// open file. Each oversized regular file is rewritten in place with its newest
// maxBytes tail, so a process holding the original descriptor is never handed
// a deleted path. Held files are deferred to a later pass.
func RotateLogs(dataDir string, maxBytes int64) (int, error) {
	if maxBytes <= 0 || dataDir == "" {
		return 0, nil
	}
	rotated := 0
	var errs []error
	for _, name := range knownLogNames {
		path := filepath.Join(dataDir, name)
		info, err := os.Lstat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			errs = append(errs, fmt.Errorf("stat %s: %w", path, err))
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			continue
		}
		if info.Size() <= maxBytes {
			continue
		}
		inUse, probeErr := openFileProbe(path)
		if probeErr != nil {
			errs = append(errs, fmt.Errorf("probe %s: %w", path, probeErr))
			continue
		}
		if inUse {
			continue
		}
		if err := rotateLog(path, maxBytes); err != nil {
			errs = append(errs, fmt.Errorf("rotate %s: %w", path, err))
			continue
		}
		rotated++
	}
	return rotated, errors.Join(errs...)
}

func rotateLog(path string, maxBytes int64) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer file.Close() //nolint:errcheck
	current, err := file.Stat()
	if err != nil {
		return err
	}
	if current.Size() <= maxBytes {
		return nil
	}
	start := current.Size() - maxBytes
	tail := make([]byte, maxBytes)
	if _, err := file.ReadAt(tail, start); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if err := file.Truncate(0); err != nil {
		return err
	}
	if len(tail) > 0 {
		if _, err := file.WriteAt(tail, 0); err != nil {
			return err
		}
	}
	return file.Sync()
}

// ReapStaleProcessFiles removes dead Ghost process claims and orphan lock
// siblings from the retired per-phase naming scheme. Current lifecycle and
// Obsidian claim locks are persistent by design, so their lock sidecars are
// left in place. Liveness is checked before taking a legacy claim lock; a live
// PID is never removed, and a lock held by another process is left for its
// owner.
func ReapStaleProcessFiles(dataDir string) (int, error) {
	if dataDir == "" {
		return 0, nil
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read data dir %s: %w", dataDir, err)
	}

	removed := 0
	var errs []error
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		name := entry.Name()
		switch {
		case isGhostPIDName(name):
			n, err := removeStaleClaim(filepath.Join(dataDir, name))
			removed += n
			if err != nil {
				errs = append(errs, err)
			}
		case isGhostPIDLockName(name):
			pidName := strings.TrimSuffix(name, ".lock")
			if !isLegacyPIDName(pidName) {
				continue // current claim locks are intentionally persistent
			}
			if _, statErr := os.Stat(filepath.Join(dataDir, pidName)); statErr == nil {
				continue // the PID pass owns the paired lock
			} else if !os.IsNotExist(statErr) {
				errs = append(errs, fmt.Errorf("stat %s: %w", pidName, statErr))
				continue
			}
			n, err := removeOrphanLock(filepath.Join(dataDir, name))
			removed += n
			if err != nil {
				errs = append(errs, err)
			}
		}
	}
	return removed, errors.Join(errs...)
}

func isGhostPIDName(name string) bool {
	if name == "obsidian-sync.pid" {
		return true
	}
	if strings.HasPrefix(name, "lifecycle-") && strings.HasSuffix(name, ".pid") && len(name) > len("lifecycle-.pid") {
		return true
	}
	return isLegacyPIDName(name)
}

func isLegacyPIDName(name string) bool {
	for _, prefix := range []string{"reflect-", "resolve-", "supersede-"} {
		if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ".pid") && len(name) > len(prefix)+len(".pid") {
			return true
		}
	}
	return false
}

func isGhostPIDLockName(name string) bool {
	return strings.HasSuffix(name, ".pid.lock") && isGhostPIDName(strings.TrimSuffix(name, ".lock"))
}

func removeStaleClaim(pidPath string) (int, error) {
	alive, known, err := claimIsAlive(pidPath)
	if err != nil {
		return 0, err
	}
	if known && alive {
		return 0, nil
	}
	if !known {
		if _, statErr := os.Lstat(pidPath); os.IsNotExist(statErr) {
			return 0, nil
		} else if statErr != nil {
			return 0, fmt.Errorf("stat %s: %w", pidPath, statErr)
		}
	}

	lockPath := pidPath + ".lock"
	lock, err := openProcessLock(lockPath)
	if err != nil {
		return 0, err
	}
	defer lock.Close() //nolint:errcheck
	locked, err := tryLockExclusive(lock)
	if err != nil {
		return 0, fmt.Errorf("lock %s: %w", lockPath, err)
	}
	if !locked {
		return 0, nil // another Ghost process owns the claim
	}
	defer unlockProcessLock(lock) //nolint:errcheck

	// Re-check under the lock: a claimant may have replaced the file between
	// the initial liveness read and lock acquisition.
	alive, known, err = claimIsAlive(pidPath)
	if err != nil {
		return 0, err
	}
	if known && alive {
		return 0, nil
	}
	inUse, probeErr := openFileProbe(pidPath)
	if probeErr != nil {
		return 0, fmt.Errorf("probe PID file %s: %w", pidPath, probeErr)
	}
	if inUse {
		return 0, nil
	}
	removed := 0
	pidExisted := !fileIsMissing(pidPath)
	if err := removeRegularFile(pidPath); err != nil {
		return removed, err
	}
	if pidExisted {
		removed++
	}
	_ = removeRegularFile(pidPath + ".tmp")
	if isLegacyPIDName(filepath.Base(pidPath)) {
		if err := removeOwnedLock(lockPath, lock); err != nil {
			return removed, err
		}
	}
	return removed, nil
}

func removeOrphanLock(lockPath string) (int, error) {
	inUse, probeErr := openFileProbe(lockPath)
	if probeErr != nil {
		return 0, fmt.Errorf("probe orphan lock %s: %w", lockPath, probeErr)
	}
	if inUse {
		return 0, nil
	}
	lock, err := openProcessLock(lockPath)
	if err != nil {
		return 0, err
	}
	defer lock.Close() //nolint:errcheck
	locked, err := tryLockExclusive(lock)
	if err != nil {
		return 0, fmt.Errorf("lock %s: %w", lockPath, err)
	}
	if !locked {
		return 0, nil
	}
	defer unlockProcessLock(lock) //nolint:errcheck
	if err := removeOwnedLock(lockPath, lock); err != nil {
		return 0, err
	}
	return 1, nil
}

func openProcessLock(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("process lock is not a regular file: %s", path)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat process lock %s: %w", path, err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open process lock %s: %w", path, err)
	}
	return file, nil
}

func claimIsAlive(path string) (alive, known bool, err error) {
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("read PID file %s: %w", path, readErr)
	}
	pidText, token, haveToken := strings.Cut(strings.TrimSpace(string(data)), ":")
	pid, parseErr := strconv.Atoi(pidText)
	if parseErr != nil || pid <= 0 {
		return false, false, nil
	}
	return procstat.IsAlive(pid, token, haveToken), true, nil
}

func removeRegularFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("refusing to remove non-regular file %s", path)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

func fileIsMissing(path string) bool {
	_, err := os.Lstat(path)
	return os.IsNotExist(err)
}

func detectOpenFile(path string) (bool, error) {
	tool, err := exec.LookPath("lsof")
	if err != nil {
		tool, err = exec.LookPath("fuser")
	}
	if err != nil {
		return false, errors.New("neither lsof nor fuser is available")
	}
	runErr := exec.Command(tool, path).Run()
	if runErr == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("%s: %w", filepath.Base(tool), runErr)
}
