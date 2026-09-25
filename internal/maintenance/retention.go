// Package maintenance owns bounded, best-effort cleanup of Ghost-owned data-dir
// files and the shared race-safe primitive used to reclaim orphaned export
// temporaries. Data-dir retention operates on an explicit directory and
// database path: it must never create a data directory as a side effect of a
// status, hook, or failed cleanup pass.
package maintenance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/procstat"
)

const (
	openProbeTimeout = 2 * time.Second
	quarantineGrace  = time.Hour
)

var (
	// openFileProbe is a narrow seam for tests and for the production
	// lsof/fuser probe with a native fallback. A probe error is fail-closed:
	// the candidate is left alone.
	openFileProbe = detectOpenFile

	runOpenProbeTool = func(ctx context.Context, tool, path string) error {
		return exec.CommandContext(ctx, tool, path).Run()
	}

	knownLogNames = []string{
		"lifecycle.log",
		"obsidian-sync.log",
		"reflect.log",
		"resolve.log",
		"supersede.log",
	}

	quarantineNamePattern = regexp.MustCompile(`^\..+\.retention-[A-Za-z0-9]+$`)
)

// OpenFileProbe reports whether a path is held by a process. It is exported so
// tests in dependent packages can pin the probe instead of inheriting whatever
// lsof, fuser, or procfs state the host happens to have.
type OpenFileProbe func(string) (bool, error)

// SetOpenFileProbeForTest installs a deterministic probe and returns a restore
// function. It is a test seam inside an internal package, not a runtime option.
func SetOpenFileProbeForTest(probe OpenFileProbe) func() {
	old := openFileProbe
	if probe == nil {
		openFileProbe = detectOpenFile
	} else {
		openFileProbe = probe
	}
	return func() { openFileProbe = old }
}

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
		if entry.IsDir() || !quarantineNamePattern.MatchString(entry.Name()) {
			continue
		}
		path := filepath.Join(dataDir, entry.Name())
		info, statErr := os.Lstat(path)
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			continue
		}
		if time.Since(info.ModTime()) < quarantineGrace {
			continue
		}
		ok, removeErr := removeUnheldFile(path)
		if removeErr != nil {
			errs = append(errs, removeErr)
			continue
		}
		if ok {
			removed++
		}
	}
	return removed, errors.Join(errs...)
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
	inUse, probeErr := openFileProbe(path)
	if probeErr != nil {
		return false, probeErr
	}
	if inUse {
		return false, nil
	}
	tombstone, err := quarantinePath(path)
	if err != nil {
		if renameMeansHeld(err) {
			return false, nil
		}
		return false, err
	}
	inUse, probeErr = openFileProbe(tombstone)
	if probeErr != nil {
		return false, errorsJoinRestore(path, tombstone, probeErr)
	}
	if inUse {
		return false, errorsJoinRestore(path, tombstone, nil)
	}
	tail, err := readLogTail(tombstone, maxBytes)
	if err != nil {
		return false, errorsJoinRestore(path, tombstone, err)
	}
	out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if errors.Is(err, os.ErrExist) {
		// A writer recreated the visible path after quarantine. Preserve that
		// writer's file, bound the old inode in place, and let the grace-period
		// tombstone reaper remove the hidden copy later.
		if rewriteErr := rewriteQuarantine(tombstone, tail); rewriteErr != nil {
			return false, rewriteErr
		}
		return false, nil
	}
	if err != nil {
		return false, errorsJoinRestore(path, tombstone, err)
	}
	if _, err := out.Write(tail); err != nil {
		_ = out.Close()
		_ = os.Remove(path)
		return false, errorsJoinRestore(path, tombstone, err)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(path)
		return false, errorsJoinRestore(path, tombstone, err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(path)
		return false, errorsJoinRestore(path, tombstone, err)
	}
	if err := removeQuarantine(tombstone); err != nil {
		return false, err
	}
	return true, nil
}

func rewriteQuarantine(path string, tail []byte) error {
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
	lock, err := openProcessLock(lockPath)
	if err != nil {
		return 0, err
	}
	lockHeld, lockOpen := true, true
	defer func() {
		if lockHeld {
			_ = unlockProcessLock(lock)
		}
		if lockOpen {
			_ = lock.Close()
		}
	}()
	locked, err := tryLockExclusive(lock)
	if err != nil {
		return 0, fmt.Errorf("lock %s: %w", lockPath, err)
	}
	if !locked {
		return 0, nil
	}

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
	_ = unlockProcessLock(lock)
	lockHeld = false
	_ = lock.Close()
	lockOpen = false
	if removedLock, err := removeUnheldFile(lockPath); err != nil {
		return removed, err
	} else if removedLock {
		removed++
	}
	return removed, nil
}

func removeOrphanLock(lockPath string) (int, error) {
	lock, err := openProcessLock(lockPath)
	if err != nil {
		return 0, err
	}
	locked, err := tryLockExclusive(lock)
	if err != nil {
		_ = lock.Close()
		return 0, fmt.Errorf("lock %s: %w", lockPath, err)
	}
	if !locked {
		_ = lock.Close()
		return 0, nil
	}
	_ = unlockProcessLock(lock)
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

func claimState(path string) (procstat.State, bool, error) {
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

// RemoveFileIfUnheld removes a regular file only after atomically moving it
// to a private quarantine name and proving that no process holds that inode.
// It is exported for the Obsidian orphan-temp reaper so it shares the same
// race-safe deletion boundary as data-dir retention.
func RemoveFileIfUnheld(path string) (bool, error) {
	return removeUnheldFile(path)
}

func detectOpenFile(path string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), openProbeTimeout)
	defer cancel()

	tool, err := exec.LookPath("lsof")
	if err != nil {
		tool, err = exec.LookPath("fuser")
	}
	var toolErr error
	if err == nil {
		inUse, known, probeErr := probeOpenFileWithTool(ctx, tool, path)
		if known {
			return inUse, probeErr
		}
		toolErr = probeErr
	}
	if inUse, known, nativeErr := nativeOpenProbe(path); known {
		return inUse, nativeErr
	}
	if err != nil {
		return false, errors.New("neither a native open-file probe nor lsof/fuser is available")
	}
	return false, toolErr
}

func probeOpenFileWithTool(ctx context.Context, tool, path string) (inUse, known bool, err error) {
	runErr := runOpenProbeTool(ctx, tool, path)
	if runErr == nil {
		return true, true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) && exitErr.ExitCode() == 1 {
		return false, true, nil
	}
	return false, false, fmt.Errorf("%s: %w", filepath.Base(tool), runErr)
}
