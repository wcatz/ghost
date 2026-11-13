package ai

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

func TestHarnessCommand_EnforcesScratchBudgetBeforeSpawn(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GHOST_SCRATCH_DIR", root)
	t.Setenv("GHOST_SCRATCH_MAX_BYTES", "4096")

	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

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

	foreign := filepath.Join(root, "foreign.bin")
	if err := os.WriteFile(foreign, make([]byte, 8192), 0o600); err != nil {
		t.Fatalf("write foreign file: %v", err)
	}

	logs := captureProcessLogs(t)

	cmd, release, err := harnessCommand(context.Background(), "true", nil, nil, harnessClaude)
	t.Cleanup(release)

	if err != nil || cmd == nil {
		t.Fatalf("budget enforcement blocked the spawn: cmd=%v err=%v", cmd, err)
	}
	if !strings.HasPrefix(cmd.Dir, root) {
		t.Errorf("cmd.Dir = %q, want a dir under %q (spawn not confined)", cmd.Dir, root)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale dir survived the budget reaper (stat err %v)", err)
	}

	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("foreign file must survive Reap: %v", err)
	}

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
