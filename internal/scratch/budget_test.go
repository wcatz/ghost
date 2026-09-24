package scratch

import (
	"bytes"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// TestSize_CountsRegularFilesOnly: Size accounts bytes + files over regular
// files with lstat semantics — symlinks are never followed (a link to a large
// out-of-tree file contributes neither bytes nor file counts), directories
// count zero, and a missing root is (0, 0, nil).
func TestSize_CountsRegularFilesOnly(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.bin"), make([]byte, 1000), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "b.bin"), make([]byte, 2000), 0o600); err != nil {
		t.Fatal(err)
	}
	// A symlink whose target lives outside the root: must not be followed.
	outside := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(outside, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link.bin")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	gotBytes, gotFiles, err := Size(root)
	if err != nil {
		t.Fatalf("Size: %v", err)
	}
	if gotBytes != 3000 {
		t.Errorf("Size bytes = %d, want 3000 (symlink target excluded)", gotBytes)
	}
	if gotFiles != 2 {
		t.Errorf("Size files = %d, want 2 (symlink excluded)", gotFiles)
	}

	gotBytes, gotFiles, err = Size(filepath.Join(root, "does-not-exist"))
	if err != nil {
		t.Errorf("Size on missing root: err = %v, want nil", err)
	}
	if gotBytes != 0 || gotFiles != 0 {
		t.Errorf("Size on missing root = (%d, %d), want (0, 0)", gotBytes, gotFiles)
	}
}

// captureLogs redirects the package-level default logger for the test's
// duration so budget warnings are assertable byte-for-byte.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestEnforceBudget_UnderBudgetSilentAndSkipsReap: a root within budget must
// produce no warning and no maintenance_runs row — and, per the controller's
// fixed order (measure → only-if-over: reap), NO reap: the stale markerless
// dir survives because the lifecycle/init reaps own unconditional hygiene.
// The database is deliberately absent here too: the under-budget path must not
// need or create one.
func TestEnforceBudget_UnderBudgetSilentAndSkipsReap(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GHOST_SCRATCH_DIR", root)
	t.Setenv("GHOST_SCRATCH_MAX_BYTES", "1048576") // 1 MiB — plenty for the fixture
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// Stale markerless scratch-shaped dir, older than the 24h age bound: would
	// be reaped if the check fired, must survive because it did not.
	stale := filepath.Join(root, "123-0123456789abcdef")
	if err := os.Mkdir(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "payload"), make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	logs := captureLogs(t)
	res := EnforceBudget()

	if res.Fired {
		t.Errorf("EnforceBudget fired under budget: %+v", res)
	}
	if res.ReapedCount != 0 {
		t.Errorf("ReapedCount = %d, want 0 (no reap under budget)", res.ReapedCount)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Errorf("stale dir reaped although under budget: %v", err)
	}
	if out := logs.String(); strings.Contains(out, "over budget") {
		t.Errorf("warning emitted under budget:\n%s", out)
	}
}

// TestEnforceBudget_OverBudgetReapsWarnsAndRecords: the full fire path. Over
// budget → reap stale → STILL over (foreign file kept) → loud warning naming
// root and over-bytes → a maintenance_runs row with post-reap scratch bytes
// and reaped counts. Returns normally either way (never blocks).
func TestEnforceBudget_OverBudgetReapsWarnsAndRecords(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GHOST_SCRATCH_DIR", root)
	t.Setenv("GHOST_SCRATCH_MAX_BYTES", "4096")
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// The database exists (record target); created here exactly as bootstrap does.
	if err := os.MkdirAll(filepath.Join(dataHome, "ghost"), 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dataHome, "ghost", "ghost.db")
	db, err := openTestDB(dbPath)
	if err != nil {
		t.Fatalf("seed db: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	stale := filepath.Join(root, "123-0123456789abcdef")
	if err := os.Mkdir(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "payload"), make([]byte, 8192), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(root, "foreign.bin")
	if err := os.WriteFile(foreign, make([]byte, 8192), 0o600); err != nil {
		t.Fatal(err)
	}

	logs := captureLogs(t)
	res := EnforceBudget()

	if !res.Fired {
		t.Fatalf("budget check did not fire over budget: %+v", res)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale dir survived the reaper (stat err %v)", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("foreign file must survive Reap: %v", err)
	}
	out := logs.String()
	for _, want := range []string{"over budget", root, "max_bytes=4096", "over_by=4096"} {
		if !strings.Contains(out, want) {
			t.Errorf("warning missing %q; slog output:\n%s", want, out)
		}
	}
	if !res.OverBudget {
		t.Errorf("OverBudget = false, want true (8192 > 4096 after reap)")
	}
	if res.ReapedCount != 1 || res.ReapedBytes != 8192 {
		t.Errorf("reaped = (%d, %d), want (1, 8192)", res.ReapedCount, res.ReapedBytes)
	}
	if !res.Recorded {
		t.Errorf("Recorded = false, want true (fire path writes a maintenance_runs row)")
	}

	// The row itself.
	db, err = openTestDB(dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer db.Close() //nolint:errcheck
	var kind, note string
	var scratchBytes, reapedBytes, reapedCount int
	if err := db.QueryRow(
		`SELECT kind, scratch_bytes, scratch_reaped_bytes, scratch_reaped_count, note
		 FROM maintenance_runs`,
	).Scan(&kind, &scratchBytes, &reapedBytes, &reapedCount, &note); err != nil {
		t.Fatalf("select maintenance_runs: %v", err)
	}
	if kind != "scratch-budget" {
		t.Errorf("kind = %q, want scratch-budget", kind)
	}
	if scratchBytes != 8192 {
		t.Errorf("scratch_bytes = %d, want 8192 (post-reap)", scratchBytes)
	}
	if reapedBytes != 8192 || reapedCount != 1 {
		t.Errorf("reaped = (%d B, %d), want (8192, 1)", reapedBytes, reapedCount)
	}
	if !strings.Contains(note, "over budget by 4096 bytes") {
		t.Errorf("note = %q, want it to carry the over-budget delta", note)
	}
}

// TestEnforceBudget_ReapIntoBudgetRecordsWithoutWarning: over budget only
// because of stale content — the reap brings the root back within budget, so
// there is nothing to warn about, but the fired check still records its reaped
// counts (the "reaped spawn" the status command must show).
func TestEnforceBudget_ReapIntoBudgetRecordsWithoutWarning(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GHOST_SCRATCH_DIR", root)
	t.Setenv("GHOST_SCRATCH_MAX_BYTES", "4096")
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if err := os.MkdirAll(filepath.Join(dataHome, "ghost"), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := openTestDB(filepath.Join(dataHome, "ghost", "ghost.db"))
	if err != nil {
		t.Fatalf("seed db: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	stale := filepath.Join(root, "123-0123456789abcdef")
	if err := os.Mkdir(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "payload"), make([]byte, 8192), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	logs := captureLogs(t)
	res := EnforceBudget()

	if !res.Fired {
		t.Fatalf("check did not fire: %+v", res)
	}
	if res.OverBudget {
		t.Errorf("OverBudget = true, want false (root emptied by reap)")
	}
	if out := logs.String(); strings.Contains(out, "over budget") {
		t.Errorf("warning emitted although the reap restored budget:\n%s", out)
	}
	if !res.Recorded || res.ReapedCount != 1 {
		t.Errorf("recorded=%v reaped=%d, want recorded with 1 reaped entry", res.Recorded, res.ReapedCount)
	}
}

// TestEnforceBudget_DisabledWhenZero: max_bytes=0 is the explicit opt-out —
// no measure-driven reap, no warning, no row, even for a wildly over-budget
// root. (The stale dir survives exactly as in the under-budget case.)
func TestEnforceBudget_DisabledWhenZero(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GHOST_SCRATCH_DIR", root)
	t.Setenv("GHOST_SCRATCH_MAX_BYTES", "0")
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	stale := filepath.Join(root, "123-0123456789abcdef")
	if err := os.Mkdir(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "payload"), make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	logs := captureLogs(t)
	res := EnforceBudget()

	if res.Fired || res.ReapedCount != 0 || res.Recorded {
		t.Errorf("disabled budget still acted: %+v", res)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Errorf("stale dir reaped although enforcement disabled: %v", err)
	}
	if out := logs.String(); strings.Contains(out, "over budget") {
		t.Errorf("warning emitted although enforcement disabled:\n%s", out)
	}
}

// openTestDB opens (creating if needed) the ghost database at dbPath the same
// way bootstrap does, so budget-record tests exercise the real schema.
func openTestDB(dbPath string) (*sql.DB, error) {
	return memory.OpenDB(dbPath)
}
