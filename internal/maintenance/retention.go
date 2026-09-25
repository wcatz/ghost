// Package maintenance owns bounded, best-effort cleanup of Ghost-owned data-dir
// files and the shared race-safe primitive used to reclaim orphaned export
// temporaries. Data-dir retention operates on an explicit directory and
// database path: it must never create a data directory as a side effect of a
// status, hook, or failed cleanup pass.
package maintenance

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/fileguard"
	"github.com/wcatz/ghost/internal/procstat"
)

const quarantineGrace = fileguard.QuarantineGrace

var (
	// openFileProbe is a narrow test seam. Production uses fileguard's bounded
	// lsof/fuser probe with native fallback.
	openFileProbe fileguard.OpenFileProbe = fileguard.DetectOpenFile

	knownLogNames = []string{
		"lifecycle.log",
		"obsidian-sync.log",
		"reflect.log",
		"resolve.log",
		"supersede.log",
	}

	beforePublishLog = func(string) {}
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
var loadConfig = config.Load

func Run(dataDir, dbPath string) (Result, error) {
	cfg, err := loadConfig()
	if err != nil {
		result, runErr := RunWithConfig(dataDir, dbPath, config.DefaultRetentionBackupCount, config.DefaultRetentionLogMaxBytes)
		return result, errors.Join(fmt.Errorf("load retention config; using defaults: %w", err), runErr)
	}
	return RunWithConfig(dataDir, dbPath, cfg.Retention.BackupCount, cfg.Retention.LogMaxBytes)
}

// RunWithConfig performs retention with already-loaded settings. Keeping the
// settings explicit lets callers that have a Config avoid a second config load
// while tests can pin the safety boundaries without touching global env state.
func RunWithConfig(dataDir, dbPath string, backupCount int, logMaxBytes int64) (Result, error) {
	var result Result
	var errs []error

	quarantineRemoved, err := ReapStaleQuarantineFiles(dataDir)
	result.StaleFilesRemoved += quarantineRemoved
	if err != nil {
		errs = append(errs, fmt.Errorf("reap stale quarantine files: %w", err))
	}
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
	result.StaleFilesRemoved += stale
	if err != nil {
		errs = append(errs, fmt.Errorf("reap stale process files: %w", err))
	}
	return result, errors.Join(errs...)
}

// backupCandidate is a regular, timestamped pre-migrate copy. The numeric
// suffix is the migration timestamp and is authoritative; mtime only breaks a
// timestamp tie.
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
	if info, statErr := os.Stat(dbPath); statErr == nil {
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
			continue // prefix debris is not a retention candidate
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
		if candidates[i].stamp != candidates[j].stamp {
			return candidates[i].stamp > candidates[j].stamp
		}
		if candidates[i].mtime != candidates[j].mtime {
			return candidates[i].mtime > candidates[j].mtime
		}
		return candidates[i].name > candidates[j].name
	})
	if len(candidates) <= keep {
		return 0, nil
	}

	removed := 0
	var errs []error
	for _, candidate := range candidates[keep:] {
		ok, err := removeUnheldFile(candidate.path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if ok {
			removed++
		}
	}
	return removed, errors.Join(errs...)
}

// ReapStaleQuarantineFiles removes provably unheld quarantine tombstones left
// by an interrupted or racing rotation once they are older than a grace period.
func ReapStaleQuarantineFiles(dataDir string) (int, error) {
	if dataDir == "" {
		return 0, nil
	}
	return fileguard.ReapStaleQuarantineWithProbe(dataDir, openFileProbe)
}

// RotateLogs bounds the known Ghost-owned data-dir logs without deleting an
// open file. An oversized file is atomically quarantined first, so a writer
// that races the probe either follows the new path or is detected on the
// quarantined inode before it is removed.
func RotateLogs(dataDir string, maxBytes int64) (int, error) {
	if maxBytes <= 0 || dataDir == "" {
		return 0, nil
	}
	rotated := 0
	var errs []error
	for _, name := range knownLogNames {
		ok, err := rotateLog(filepath.Join(dataDir, name), maxBytes)
		if err != nil {
			errs = append(errs, fmt.Errorf("rotate %s: %w", name, err))
			continue
		}
		if ok {
			rotated++
		}
	}
	return rotated, errors.Join(errs...)
}

func rotateLog(path string, maxBytes int64) (bool, error) {
	lock, acquired, err := fileguard.TryAcquireLock(path + ".lock")
	if err != nil {
		return false, err
	}
	if !acquired {
		return false, nil
	}
	defer lock.Close() //nolint:errcheck

	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() <= maxBytes {
		return false, nil
	}
	tombstone, err := fileguard.Quarantine(path, openFileProbe)
	if errors.Is(err, fileguard.ErrHeld) || os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	tail, err := readLogTail(tombstone, maxBytes)
	if err != nil {
		return false, errors.Join(err, fileguard.Restore(path, tombstone))
	}
	stage, err := fileguard.StageFile(path)
	if err != nil {
		return false, errors.Join(err, fileguard.Restore(path, tombstone))
	}
	stagePath := stage.Name()
	defer os.Remove(stagePath) //nolint:errcheck
	if _, err := stage.Write(tail); err != nil {
		_ = stage.Close()
		return false, errors.Join(err, fileguard.Restore(path, tombstone))
	}
	if err := stage.Sync(); err != nil {
		_ = stage.Close()
		return false, errors.Join(err, fileguard.Restore(path, tombstone))
	}
	if err := stage.Close(); err != nil {
		return false, errors.Join(err, fileguard.Restore(path, tombstone))
	}
	beforePublishLog(path)
	if err := fileguard.PublishNoReplace(stagePath, path); errors.Is(err, fileguard.ErrPublishExists) {
		// A non-cooperating writer recreated the visible path. Preserve it,
		// bound the old inode in place, and let the tombstone reaper remove it.
		if rewriteErr := fileguard.RewriteQuarantine(tombstone, tail); rewriteErr != nil {
			return false, rewriteErr
		}
		return false, nil
	} else if err != nil {
		return false, errors.Join(err, fileguard.Restore(path, tombstone))
	}
	if err := fileguard.RemoveQuarantine(tombstone); err != nil {
		return false, err
	}
	return true, nil
}

func readLogTail(path string, maxBytes int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close() //nolint:errcheck
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() <= maxBytes {
		return nil, nil
	}
	start := info.Size() - maxBytes
	tail := make([]byte, maxBytes)
	if _, err := file.ReadAt(tail, start); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return tail, nil
}

// ReapStaleProcessFiles removes only the retired per-phase claim names. The
// current lifecycle and Obsidian protocols are deliberately outside this
// reaper: their lock inodes are persistent, their PID files are updated after
// process start, and a killed lifecycle coordinator can still have phase
// children running.
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
		case isLegacyPIDName(name):
			n, err := removeStaleClaim(filepath.Join(dataDir, name))
			removed += n
			if err != nil {
				errs = append(errs, err)
			}
		case isLegacyPIDLockName(name):
			pidName := strings.TrimSuffix(name, ".lock")
			if _, statErr := os.Stat(filepath.Join(dataDir, pidName)); statErr == nil {
				continue // the paired PID pass owns it
			} else if !os.IsNotExist(statErr) {
				errs = append(errs, fmt.Errorf("stat %s: %w", pidName, statErr))
				continue
			}
			n, err := removeOrphanLock(filepath.Join(dataDir, name))
			removed += n
			if err != nil {
				errs = append(errs, err)
			}
		case isLegacyPIDTempName(name):
			pidName := strings.TrimSuffix(name, ".tmp")
			if _, statErr := os.Stat(filepath.Join(dataDir, pidName)); statErr == nil {
				continue // the paired PID pass owns it
			} else if !os.IsNotExist(statErr) {
				errs = append(errs, fmt.Errorf("stat %s: %w", pidName, statErr))
				continue
			}
			n, err := removeOrphanTemp(filepath.Join(dataDir, name))
			removed += n
			if err != nil {
				errs = append(errs, err)
			}
		}
	}
	return removed, errors.Join(errs...)
}

func isLegacyPIDName(name string) bool {
	for _, prefix := range []string{"reflect-", "resolve-", "supersede-"} {
		if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ".pid") && len(name) > len(prefix)+len(".pid") {
			return true
		}
	}
	return false
}

func isLegacyPIDLockName(name string) bool {
	return strings.HasSuffix(name, ".pid.lock") && isLegacyPIDName(strings.TrimSuffix(name, ".lock"))
}

func isLegacyPIDTempName(name string) bool {
	return strings.HasSuffix(name, ".pid.tmp") && isLegacyPIDName(strings.TrimSuffix(name, ".tmp"))
}

func removeStaleClaim(pidPath string) (int, error) {
	state, known, err := claimState(pidPath)
	if err != nil {
		return 0, err
	}
	if !known || state != procstat.StateDead {
		return 0, nil
	}

	lockPath := pidPath + ".lock"
	lock, acquired, err := fileguard.TryAcquireLock(lockPath)
	if err != nil {
		return 0, fmt.Errorf("lock %s: %w", lockPath, err)
	}
	if !acquired {
		return 0, nil
	}
	defer lock.Close() //nolint:errcheck

	state, known, err = claimState(pidPath)
	if err != nil {
		return 0, err
	}
	if !known || state != procstat.StateDead {
		return 0, nil
	}
	removedPID, err := removeUnheldFile(pidPath)
	if err != nil {
		return 0, err
	}
	if !removedPID {
		return 0, nil
	}
	removed := 1
	if removedTemp, err := removeUnheldFile(pidPath + ".tmp"); err != nil {
		return removed, err
	} else if removedTemp {
		removed++
	}

	// Only retired names reach this path. Close the legacy lock before
	// quarantining its inode; current protocol locks are never candidates.
	_ = lock.Close()
	if removedLock, err := removeUnheldFile(lockPath); err != nil {
		return removed, err
	} else if removedLock {
		removed++
	}
	return removed, nil
}

func removeOrphanLock(lockPath string) (int, error) {
	lock, acquired, err := fileguard.TryAcquireLock(lockPath)
	if err != nil {
		return 0, fmt.Errorf("lock %s: %w", lockPath, err)
	}
	if !acquired {
		return 0, nil
	}
	_ = lock.Close()
	removed, err := removeUnheldFile(lockPath)
	if err != nil {
		return 0, err
	}
	if removed {
		return 1, nil
	}
	return 0, nil
}

func removeOrphanTemp(path string) (int, error) {
	removed, err := removeUnheldFile(path)
	if err != nil {
		return 0, err
	}
	if removed {
		return 1, nil
	}
	return 0, nil
}

func claimState(path string) (procstat.State, bool, error) {
	info, statErr := os.Lstat(path)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return procstat.StateDead, false, nil
		}
		return procstat.StateUnknown, false, fmt.Errorf("stat PID file %s: %w", path, statErr)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > 4096 {
		return procstat.StateUnknown, false, nil
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return procstat.StateDead, false, nil
		}
		return procstat.StateUnknown, false, fmt.Errorf("read PID file %s: %w", path, readErr)
	}
	pidText, token, haveToken := strings.Cut(strings.TrimSpace(string(data)), ":")
	pid, parseErr := strconv.Atoi(pidText)
	if parseErr != nil || pid <= 0 {
		return procstat.StateDead, true, nil
	}
	return procstat.Check(pid, token, haveToken), true, nil
}

func removeUnheldFile(path string) (bool, error) {
	return fileguard.RemoveIfUnheldWithProbe(path, openFileProbe)
}
