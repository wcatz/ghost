package scratch

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
)

const staleAge = 24 * time.Hour

// BudgetResult reports what one pre-spawn scratch budget check did. It is a
type BudgetResult struct {
	Root        string
	MaxBytes    int64
	BytesBefore int64
	BytesAfter  int64
	ReapedCount int
	ReapFailed  int
	Skipped     int
	ReapedBytes int64
	OverBudget  bool // still over budget after reaping
	Fired       bool // initial measure exceeded the budget (reap path ran)
	Recorded    bool // a maintenance_runs row was written
	ReapErr     error
}

// Size walks dir and returns the total bytes and count of regular files it
func Size(dir string) (bytes int64, files int, skipped int, err error) {
	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) && path == dir {
				return nil // root itself absent: empty, not an error
			}
			if path == dir {
				return walkErr // unreadable root: the measurement cannot proceed
			}
			skipped++
			if d != nil && d.IsDir() {
				return fs.SkipDir // unreadable subdirectory: measure what we can
			}
			return nil // unreadable entry: skip it
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info() // lstat semantics for symlinks
		if err != nil {
			if os.IsNotExist(err) {
				return nil // vanished mid-walk
			}
			skipped++
			return nil // cannot stat: skip it, measure what we can
		}
		if !info.Mode().IsRegular() {
			return nil // symlinks, sockets, etc.: never counted, never followed
		}
		bytes += info.Size()
		files++
		return nil
	})
	if walkErr != nil {
		return bytes, files, skipped, fmt.Errorf("size %s: %w", dir, walkErr)
	}
	if skipped > 0 {
		slog.Debug("scratch size: skipped unreadable entries", "dir", dir, "skipped", skipped)
	}
	return bytes, files, skipped, nil
}

// EnforceBudget runs the per-root scratch budget check BEFORE each harness
func EnforceBudget() BudgetResult {
	maxBytes := config.LoadForHook().Scratch.MaxBytes
	if maxBytes <= 0 {
		return BudgetResult{} // enforcement explicitly disabled
	}

	root, err := Root()
	if err != nil {
		return BudgetResult{MaxBytes: maxBytes}
	}
	before, _, skipped, err := Size(root)
	if err != nil {
		slog.Warn("scratch budget: cannot measure root; skipping check",
			"root", root, "error", err)
		return BudgetResult{Root: root, MaxBytes: maxBytes}
	}

	res := BudgetResult{Root: root, MaxBytes: maxBytes, BytesBefore: before, BytesAfter: before, Skipped: skipped}
	partial := skipped > 0
	if partial {
		slog.Warn("scratch budget: measurement skipped unreadable entries; running check on the measurable portion",
			"root", root, "skipped", skipped)
	}
	if before <= maxBytes && !partial {
		return res // within budget: no reap, no warning, no record
	}

	res.Fired = true
	removed, failed, reapErr := Reap(staleAge)
	res.ReapErr = reapErr
	if reapErr != nil {
		slog.Warn("scratch budget: reap of stale entries failed",
			"root", root, "error", reapErr)
	}
	if failed > 0 {
		slog.Warn("scratch budget: could not remove stale entries",
			"root", root, "failed", failed)
	}
	res.ReapedCount = removed
	res.ReapFailed = failed
	after, _, afterSkipped, sizeErr := Size(root)
	if sizeErr != nil {
		slog.Warn("scratch budget: cannot re-measure root after reap",
			"root", root, "error", sizeErr)
		after = before // fall back to the pre-reap measure
	}
	if afterSkipped > 0 {
		slog.Warn("scratch budget: re-measurement skipped unreadable entries",
			"root", root, "skipped", afterSkipped)
	}
	res.BytesAfter = after
	if after < before {
		res.ReapedBytes = before - after
	}

	if res.OverBudget = after > maxBytes; res.OverBudget {
		slog.Warn("scratch root over budget; proceeding anyway — maintenance must not be blocked by its own hygiene, but must never be quiet",
			"root", root,
			"bytes", after,
			"max_bytes", maxBytes,
			"over_by", after-maxBytes,
			"reaped", removed,
			"reap_failed", failed,
			"skipped", res.Skipped)
	}

	res.Recorded = recordBudgetRun(res)
	return res
}

type partialOutcome struct {
	root       string
	overBudget bool
	reapFailed bool
	reapErr    bool
}

var (
	partialMu       sync.Mutex
	partialRecorded *partialOutcome
)

func recordBudgetRun(res BudgetResult) bool {
	if res.Skipped > 0 {
		key := partialOutcome{root: res.Root, overBudget: res.OverBudget, reapFailed: res.ReapFailed > 0, reapErr: res.ReapErr != nil}
		partialMu.Lock()
		defer partialMu.Unlock()
		if partialRecorded != nil && *partialRecorded == key {
			slog.Warn("scratch budget: partial measurement already recorded; suppressing repeated rows",
				"root", res.Root, "skipped", res.Skipped)
			return false
		}
		if !writeBudgetRun(res) {
			return false
		}
		partialRecorded = &key
		return true
	}
	return writeBudgetRun(res)
}

func writeBudgetRun(res BudgetResult) bool {
	dataDir, err := config.DataDirPath()
	if err != nil {
		slog.Debug("scratch budget: no data dir, run not recorded", "error", err)
		return false
	}
	dbPath := filepath.Join(dataDir, "ghost.db")
	if _, err := os.Stat(dbPath); err != nil {
		slog.Debug("scratch budget: no database yet, run not recorded", "path", dbPath)
		return false
	}
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		slog.Warn("scratch budget: could not open database to record run", "error", err)
		return false
	}
	defer db.Close() //nolint:errcheck

	note := "within budget after reaping stale entries"
	switch {
	case res.ReapErr != nil:
		note = fmt.Sprintf("reap of stale entries failed: %v", res.ReapErr)
	case res.OverBudget:
		note = fmt.Sprintf("over budget by %d bytes; proceeding", res.BytesAfter-res.MaxBytes)
	}
	parts := []string{note}
	if res.ReapFailed > 0 {
		parts = append(parts, fmt.Sprintf("%d stale entries could not be removed", res.ReapFailed))
	}
	if res.Skipped > 0 {
		parts = append(parts,
			fmt.Sprintf("measurement skipped %d unreadable entries", res.Skipped),
			"partial measurement; repeats of this outcome are not recorded again")
	}
	note = strings.Join(parts, "; ")
	run := memory.MaintenanceRun{
		Kind:               "scratch-budget",
		ScratchBytes:       res.BytesAfter,
		ScratchReapedBytes: res.ReapedBytes,
		ScratchReapedCount: int64(res.ReapedCount),
		Note:               note,
	}
	if err := memory.RecordMaintenanceRun(context.Background(), db, run); err != nil {
		slog.Warn("scratch budget: could not record maintenance run", "error", err)
		return false
	}
	return true
}
