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
	ReapFailed  int
	// Skipped is the number of entries Size could not read in the initial
	// measure. A skipped entry is bytes nobody measured, so a partial
	// measurement is reported as partial, never compared silently.
	Skipped     int
	ReapedBytes int64
	OverBudget  bool // still over budget after reaping
	Fired       bool // initial measure exceeded the budget (reap path ran)
	Recorded    bool // a maintenance_runs row was written
	// ReapErr is the error the reap returned, if any. It is carried on the
	// result so the recorded maintenance_runs row can say the reap failed
	// instead of claiming stale entries were reaped.
	ReapErr error
}

// Size walks dir and returns the total bytes and count of regular files it
// could read, plus the number of entries it could not read (skipped).
//
// The walk is lstat-based and never follows symlinks: a symlink inside the
// root contributes neither bytes nor file count, so a link to a large
// out-of-tree file cannot inflate (or, via removal, deflate) the accounting.
// A missing directory is (0, 0, 0, nil) — the root may not exist yet, which is
// not an error anywhere this is used. An entry or subdirectory that cannot be
// read is skipped so the measurement reflects what it can: one unreadable
// subtree must not disable a caller's budget check. An unreadable root is
// returned as an error. Skipped entries leave a debug trace so the hidden
// bytes stay findable.
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
// spawn (harnessCommand is the single call site). The order is fixed:
//
//  1. measure the root;
//  2. over scratch.max_bytes OR the measurement skipped unreadable entries:
//     reap stale entries (the existing Reap, which only ever removes what
//     Ghost provably created);
//  3. re-measure; still over budget → a LOUD warning naming the root and the
//     bytes-over, via slog — the same stream harnessCommand's own warnings use;
//  4. if the check fired at all (initial measure over budget OR a partial
//     measurement), record the scratch bytes, skipped count and reaped counts
//     to maintenance_runs so `ghost maintenance status` can show them — at
//     most once per measurement outcome, so a stable partial root does not
//     add a row per spawn;
//  5. return normally. Every failure path warns and returns — enforcement
//     must never block a spawn: maintenance is not to be stopped by its own
//     hygiene, but it must never be quiet either.
//
// scratch.max_bytes <= 0 disables the check entirely (the explicit opt-out;
// an unset key is the compiled 512 MiB default — see config.ScratchConfig).
func EnforceBudget() BudgetResult {
	// LoadForHook, not Load: a broken config must not block a spawn, but the
	// fallback must not be quiet either. LoadForHook reports the failure and
	// returns the compiled default, which is this function's own fallback.
	maxBytes := config.LoadForHook().Scratch.MaxBytes
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
	before, _, skipped, err := Size(root)
	if err != nil {
		slog.Warn("scratch budget: cannot measure root; skipping check",
			"root", root, "error", err)
		return BudgetResult{Root: root, MaxBytes: maxBytes}
	}

	res := BudgetResult{Root: root, MaxBytes: maxBytes, BytesBefore: before, BytesAfter: before, Skipped: skipped}
	partial := skipped > 0
	if partial {
		// A partial measurement may hide bytes over budget: a locked
		// subtree larger than the budget must not read as within budget.
		// Fire the check anyway so the reap runs and the run is recorded.
		slog.Warn("scratch budget: measurement skipped unreadable entries; running check on the measurable portion",
			"root", root, "skipped", skipped)
	}
	if before <= maxBytes && !partial {
		return res // within budget: no reap, no warning, no record
	}

	// Over budget (or measured partially): reap stale entries FIRST, then re-measure.
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
		// Not accumulated into res.Skipped: the re-measure walks the same
		// root, so it can only skip entries the initial walk already counted.
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
			"reaped", removed,
			"reap_failed", failed,
			"skipped", res.Skipped)
	}

	res.Recorded = recordBudgetRun(res)
	return res
}

// partialOutcome is the outcome class of a partial measurement: the root and
// the facts that change what the row says. A repeat of the same class adds no
// information, so it is not recorded again; a change of class (a partial root
// that goes over budget, or starts failing reaps) is new information and is.
type partialOutcome struct {
	root       string
	overBudget bool
	reapFailed bool
	reapErr    bool
}

// partialRecorded is the last partial outcome whose row was written. It bounds
// a stable partial root (a live harness dir, or one under the 24h age, that
// stays unreadable) to one row per outcome class instead of one per spawn.
// The bound is per process: a new ghost process records its own first row.
// partialMu guards it and is held across the write so concurrent checks
// cannot both pass the test and both write.
var (
	partialMu       sync.Mutex
	partialRecorded *partialOutcome
)

// recordBudgetRun writes one maintenance_runs row for a fired budget check.
// Best-effort by design: a failure is warned (never fatal — the spawn this
// check precedes must not be delayed or denied by bookkeeping) and reported
// through the result. The row is written only when the check fired (initial
// measure over budget), so a lifecycle's hundreds of quiet spawns add zero
// rows while every reaped or warned spawn lands exactly once.
//
// A partial measurement fires the check even within budget, because its
// skipped entries could hide bytes over budget. That would add one row per
// spawn while the entry stays unreadable, so a repeat of the same partial
// outcome (see partialOutcome) is not recorded again: the warning names the
// suppression and the function returns false, since no row was written.
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

// writeBudgetRun writes the row and reports whether it was written.
func writeBudgetRun(res BudgetResult) bool {
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
