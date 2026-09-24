package ai

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// TestHarnessCommand_EnforcesScratchBudgetBeforeSpawn pins the Phase-3 budget
// contract at the single funnel every LLM harness spawn goes through
// (harnessCommand): a root that is over scratch.max_bytes must have its stale
// entries reaped BEFORE the spawn; if it is STILL over budget after reaping,
// a loud warning naming the root and the bytes-over must be emitted; and none
// of it may block the spawn — the command must still be constructed and
// confined. The budget-check event (scratch bytes + reaped counts) must land
// in maintenance_runs so `ghost maintenance status` can show it.
//
// Fixture math: budget 4096; stale reaped dir 8192 B (markerless, older than
// the 24h reap age, Open's exact name shape) + a foreign 8192 B file Reap must
// never collect → 16384 B before, 8192 B after the reap → still over by 4096.
func TestHarnessCommand_EnforcesScratchBudgetBeforeSpawn(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GHOST_SCRATCH_DIR", root)
	t.Setenv("GHOST_SCRATCH_MAX_BYTES", "4096")
	// Isolate config: a real user config must not inject a different budget.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	// Isolate the database the budget event records into.
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	dbPath := filepath.Join(dataHome, "ghost", "ghost.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}

	// Stale entry the reaper must collect: markerless, Open's exact name shape,
	// mtime older than the 24h age bound.
	stale := filepath.Join(root, "123-0123456789abcdef")
	if err := os.Mkdir(stale, 0o700); err != nil {
		t.Fatalf("mkdir stale dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stale, "payload"), make([]byte, 8192), 0o600); err != nil {
		t.Fatalf("write stale payload: %v", err)
	}
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("chtimes stale dir: %v", err)
	}
	// Foreign entry Reap must never collect — keeps the root over budget after
	// the reap so the loud-warning path fires too.
	foreign := filepath.Join(root, "foreign.bin")
	if err := os.WriteFile(foreign, make([]byte, 8192), 0o600); err != nil {
		t.Fatalf("write foreign file: %v", err)
	}

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	cmd, release, ok := harnessCommand(context.Background(), "true", nil, nil, "claude")
	t.Cleanup(release)

	// NEVER BLOCKS: the command is still constructed and confined to the root.
	if !ok || cmd == nil {
		t.Fatalf("budget enforcement blocked the spawn: cmd=%v ok=%v", cmd, ok)
	}
	if !strings.HasPrefix(cmd.Dir, root) {
		t.Errorf("cmd.Dir = %q, want a dir under %q (spawn not confined)", cmd.Dir, root)
	}

	// The stale entry was reaped as part of the pre-spawn check.
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale dir survived the budget reaper (stat err %v)", err)
	}
	// The foreign entry was correctly left alone.
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("foreign file must survive Reap: %v", err)
	}

	// STILL over budget after reaping → LOUD warning naming root + over-bytes.
	out := logs.String()
	if !strings.Contains(out, "over budget") {
		t.Errorf("no over-budget warning captured; slog output:\n%s", out)
	}
	if !strings.Contains(out, root) {
		t.Errorf("warning must name the root path %q; slog output:\n%s", root, out)
	}
	if !strings.Contains(out, "max_bytes=4096") {
		t.Errorf("warning must carry the configured budget (max_bytes=4096); slog output:\n%s", out)
	}
	if !strings.Contains(out, "over_by=4096") {
		t.Errorf("warning must carry bytes over budget (over_by=4096); slog output:\n%s", out)
	}

	// The budget-check event landed in maintenance_runs.
	db, err = memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer db.Close() //nolint:errcheck
	var count, scratchBytes, reapedCount, reapedBytes int
	if err := db.QueryRow(
		`SELECT count(*),
		        coalesce(sum(scratch_bytes), 0),
		        coalesce(sum(scratch_reaped_count), 0),
		        coalesce(sum(scratch_reaped_bytes), 0)
		 FROM maintenance_runs WHERE kind = 'scratch-budget'`,
	).Scan(&count, &scratchBytes, &reapedCount, &reapedBytes); err != nil {
		t.Fatalf("query maintenance_runs: %v", err)
	}
	if count != 1 {
		t.Errorf("maintenance_runs scratch-budget rows = %d, want 1", count)
	}
	// After the reap the root is the foreign 8192 B; the stale dir's 8192 B
	// (payload; dir inode excluded) were reaped.
	if scratchBytes != 8192 {
		t.Errorf("recorded scratch_bytes = %d, want 8192 (post-reap root size)", scratchBytes)
	}
	if reapedCount != 1 {
		t.Errorf("recorded reaped count = %d, want 1", reapedCount)
	}
	if reapedBytes != 8192 {
		t.Errorf("recorded reaped bytes = %d, want 8192", reapedBytes)
	}
}
