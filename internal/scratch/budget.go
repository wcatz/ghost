package scratch

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
)

// staleAge is how old a markerless entry must be before the budget check's
// reaper collects it — the same 24h bound the lifecycle and `ghost mcp init`
// call sites already use, so "stale" means exactly one thing everywhere.
const staleAge = 24 * time.Hour

// BudgetResult reports what one pre-spawn scratch budget check did. It is a
// value returned for tests and callers that care; EnforceBudget never fails a
// spawn on account of it.
type BudgetResult struct {
	Root        string
	MaxBytes    int64
	BytesBefore int64
	BytesAfter  int64
	ReapedCount int
	ReapedBytes int64
	OverBudget  bool // still over budget after reaping
	Fired       bool // initial measure exceeded the budget (reap path ran)
	Recorded    bool // a maintenance_runs row was written
	// ReapErr is the error the reap returned, if any. It is carried on the
	// result so the recorded maintenance_runs row can say the reap failed
	// instead of claiming stale entries were reaped.
	ReapErr error
}

// Size walks dir and returns the total bytes and count of regular files.
//
// The walk is lstat-based and never follows symlinks: a symlink inside the
// root contributes neither bytes nor a file count, so a link to a large
// out-of-tree file cannot inflate (or, via removal, deflate) the accounting.
// A missing directory is (0, 0, nil) — the root may not exist yet, which is
// not an error anywhere this is used.
func Size(dir string) (bytes int64, files int, err error) {
	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) && path == dir {
				return nil // root itself absent: empty, not an error
			}
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info() // lstat semantics for symlinks
		if err != nil {
			if os.IsNotExist(err) {
				return nil // vanished mid-walk
			}
			return err
		}
		if !info.Mode().IsRegular() {
			return nil // symlinks, sockets, etc.: never counted, never followed
		}
		bytes += info.Size()
		files++
		return nil
	})
	if walkErr != nil {
		return 0, 0, fmt.Errorf("size %s: %w", dir, walkErr)
	}
	return bytes, files, nil
}

// EnforceBudget runs the per-root scratch budget check BEFORE each harness
// spawn (harnessCommand is the single call site). The order is fixed:
//
//  1. measure the root;
//  2. only if over scratch.max_bytes: reap stale entries (the existing Reap,
//     which only ever removes what Ghost provably created);
//  3. re-measure; still over budget → a LOUD warning naming the root and the
//     bytes-over, via slog — the same stream harnessCommand's own warnings use;
//  4. if the check fired at all (initial measure over budget), record the
//     scratch bytes and reaped counts to maintenance_runs so
//     `ghost maintenance status` can show them;
//  5. return normally. Every failure path warns and returns — enforcement
//     must never block a spawn: maintenance is not to be stopped by its own
//     hygiene, but it must never be quiet either.
//
// scratch.max_bytes <= 0 disables the check entirely (the explicit opt-out;
// an unset key is the compiled 512 MiB default — see config.ScratchConfig).
func EnforceBudget() BudgetResult {
	maxBytes := config.DefaultScratchMaxBytes
	if cfg, err := config.Load(); err == nil {
		maxBytes = cfg.Scratch.MaxBytes
	} else {
		slog.Debug("scratch budget: config load failed, using compiled default",
			"error", err, "max_bytes", maxBytes)
	}
	if maxBytes <= 0 {
		return BudgetResult{} // enforcement explicitly disabled
	}

	root, err := Root()
	if err != nil {
		// Open() right after this call warns about the same root; the budget
		// check is best-effort on top of it, so a second warning here would
		// only duplicate.
		return BudgetResult{MaxBytes: maxBytes}
	}
	before, _, err := Size(root)
	if err != nil {
		slog.Warn("scratch budget: cannot measure root; skipping check",
			"root", root, "error", err)
		return BudgetResult{Root: root, MaxBytes: maxBytes}
	}

	res := BudgetResult{Root: root, MaxBytes: maxBytes, BytesBefore: before, BytesAfter: before}
	if before <= maxBytes {
		return res // within budget: no reap, no warning, no record
	}

	// Over budget: reap stale entries FIRST, then re-measure.
	res.Fired = true
	removed, reapErr := Reap(staleAge)
	res.ReapErr = reapErr
	if reapErr != nil {
		slog.Warn("scratch budget: reap of stale entries failed",
			"root", root, "error", reapErr)
	}
	res.ReapedCount = removed
	after, _, sizeErr := Size(root)
	if sizeErr != nil {
		slog.Warn("scratch budget: cannot re-measure root after reap",
			"root", root, "error", sizeErr)
		after = before // fall back to the pre-reap measure
	}
	res.BytesAfter = after
	if after < before {
		res.ReapedBytes = before - after
	}

	if res.OverBudget = after > maxBytes; res.OverBudget {
		// LOUD: the warning names the root and the exact overage. It goes to
		// the default slog logger — the stream harnessCommand's scratch-unavail
		// warning already uses — so it reaches stderr in CLI runs and
		// lifecycle.log when a phase runs detached. The spawn proceeds anyway.
		slog.Warn("scratch root over budget; proceeding anyway — maintenance must not be blocked by its own hygiene, but must never be quiet",
			"root", root,
			"bytes", after,
			"max_bytes", maxBytes,
			"over_by", after-maxBytes,
			"reaped", removed)
	}

	res.Recorded = recordBudgetRun(res)
	return res
}

// recordBudgetRun writes one maintenance_runs row for a fired budget check.
// Best-effort by design: a failure is warned (never fatal — the spawn this
// check precedes must not be delayed or denied by bookkeeping) and reported
// through the result. The row is written only when the check fired (initial
// measure over budget), so a lifecycle's hundreds of quiet spawns add zero
// rows while every reaped or warned spawn lands exactly once.
func recordBudgetRun(res BudgetResult) bool {
	dataDir, err := config.DataDirPath()
	if err != nil {
		slog.Debug("scratch budget: no data dir, run not recorded", "error", err)
		return false
	}
	dbPath := filepath.Join(dataDir, "ghost.db")
	if _, err := os.Stat(dbPath); err != nil {
		// No database yet: do not create a phantom one from a hygiene check —
		// bootstrap owns creating it.
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
		// The row is an audit trail. A failed reap must not read as a
		// successful one, whatever the post-reap measure happened to show.
		note = fmt.Sprintf("reap of stale entries failed: %v", res.ReapErr)
	case res.OverBudget:
		note = fmt.Sprintf("over budget by %d bytes; proceeding", res.BytesAfter-res.MaxBytes)
	}
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
