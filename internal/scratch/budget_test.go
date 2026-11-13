package scratch

import (
	"bytes"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// captureLogs redirects the package-level default logger for the test's
// duration so budget warnings are assertable.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// openTestDB opens (creating if needed) the ghost database at dbPath the same
// way bootstrap does, so budget-record tests exercise the real schema.
func openTestDB(dbPath string) (*sql.DB, error) {
	return memory.OpenDB(dbPath)
}

// budgetEnv points the scratch root and data dir at fresh temp dirs with the
// given scratch.max_bytes, optionally seeds the database, and returns the root
// and database path.
func budgetEnv(t *testing.T, maxBytes string, seed bool) (root, dbPath string) {
	t.Helper()
	root = t.TempDir()
	dataHome := t.TempDir()
	t.Setenv("GHOST_SCRATCH_DIR", root)
	t.Setenv("GHOST_SCRATCH_MAX_BYTES", maxBytes)
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dbPath = filepath.Join(dataHome, "ghost", "ghost.db")
	if seed {
		must(t, os.MkdirAll(filepath.Dir(dbPath), 0o700))
		db, err := openTestDB(dbPath)
		must(t, err)
		must(t, db.Close())
	}
	return root, dbPath
}

// staleEntry creates a markerless Open-shaped dir holding size payload bytes,
// aged 25h so it is past the budget reap's 24h bound.
func staleEntry(t *testing.T, root string, size int) string {
	t.Helper()
	dir := filepath.Join(root, "123-0123456789abcdef")
	must(t, os.Mkdir(dir, 0o700))
	must(t, os.WriteFile(filepath.Join(dir, "payload"), make([]byte, size), 0o600))
	old := time.Now().Add(-25 * time.Hour)
	must(t, os.Chtimes(dir, old, old))
	return dir
}

// lockDir makes dir unreadable until the test ends.
func lockDir(t *testing.T, dir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not enforced the same way on Windows")
	}
	must(t, os.Chmod(dir, 0o000))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
}

// queryRow runs query against the database at dbPath and scans one row.
func queryRow(t *testing.T, dbPath, query string, dest ...any) {
	t.Helper()
	db, err := openTestDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close() //nolint:errcheck
	if err := db.QueryRow(query).Scan(dest...); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
}

func noteOf(t *testing.T, dbPath string) string {
	t.Helper()
	var note string
	queryRow(t, dbPath, `SELECT note FROM maintenance_runs`, &note)
	return note
}

func rowCount(t *testing.T, dbPath string) int {
	t.Helper()
	var n int
	queryRow(t, dbPath, `SELECT count(*) FROM maintenance_runs`, &n)
	return n
}

// TestSize_CountsRegularFilesOnly: Size counts regular files with lstat
// semantics — symlinks are never followed, directories count zero, and a
// missing root is (0, 0, 0, nil).
func TestSize_CountsRegularFilesOnly(t *testing.T) {
	skipWindowsSymlink(t)
	root := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(root, "sub"), 0o700))
	must(t, os.WriteFile(filepath.Join(root, "a.bin"), make([]byte, 1000), 0o600))
	must(t, os.WriteFile(filepath.Join(root, "sub", "b.bin"), make([]byte, 2000), 0o600))
	outside := filepath.Join(t.TempDir(), "big.bin")
	must(t, os.WriteFile(outside, make([]byte, 1<<20), 0o600))
	must(t, os.Symlink(outside, filepath.Join(root, "link.bin")))

	b, f, s, err := Size(root)
	if err != nil || b != 3000 || f != 2 || s != 0 {
		t.Errorf("Size = (%d, %d, %d, %v), want (3000, 2, 0, nil)", b, f, s, err)
	}
	b, f, s, err = Size(filepath.Join(root, "does-not-exist"))
	if err != nil || b != 0 || f != 0 || s != 0 {
		t.Errorf("Size on missing root = (%d, %d, %d, %v), want zeros and nil", b, f, s, err)
	}
}

// TestEnforceBudget_NoActionUnderBudgetOrDisabled: a root within budget, or any
// root with max_bytes=0, produces no warning, no reap and no row. The stale dir
// survives because the lifecycle and init reaps own unconditional hygiene, and
// the database is absent: that path must not need or create one.
func TestEnforceBudget_NoActionUnderBudgetOrDisabled(t *testing.T) {
	tests := []struct {
		name, maxBytes string
		payload        int
	}{
		{"under budget", "1048576", 4096},
		{"disabled", "0", 1 << 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, dbPath := budgetEnv(t, tt.maxBytes, false)
			stale := staleEntry(t, root, tt.payload)

			logs := captureLogs(t)
			res := EnforceBudget()

			if res.Fired || res.ReapedCount != 0 || res.Recorded {
				t.Errorf("EnforceBudget acted: %+v", res)
			}
			if _, err := os.Stat(stale); err != nil {
				t.Errorf("stale dir reaped: %v", err)
			}
			if out := logs.String(); strings.Contains(out, "over budget") {
				t.Errorf("warning emitted:\n%s", out)
			}
			if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
				t.Errorf("database created on a no-action path (stat err %v)", err)
			}
		})
	}
}

// TestEnforceBudget_OverBudgetReapsWarnsAndRecords: the full fire path. Over
// budget → reap stale → STILL over (foreign file kept) → loud warning naming
// root and over-bytes → a maintenance_runs row with post-reap scratch bytes and
// reaped counts.
func TestEnforceBudget_OverBudgetReapsWarnsAndRecords(t *testing.T) {
	root, dbPath := budgetEnv(t, "4096", true)
	stale := staleEntry(t, root, 8192)
	foreign := filepath.Join(root, "foreign.bin")
	must(t, os.WriteFile(foreign, make([]byte, 8192), 0o600))

	logs := captureLogs(t)
	res := EnforceBudget()

	if !res.Fired || !res.OverBudget || !res.Recorded {
		t.Fatalf("want Fired, OverBudget and Recorded: %+v", res)
	}
	if res.ReapedCount != 1 || res.ReapedBytes != 8192 {
		t.Errorf("reaped = (%d, %d), want (1, 8192)", res.ReapedCount, res.ReapedBytes)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale dir survived the reaper (stat err %v)", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("foreign file must survive Reap: %v", err)
	}
	for _, want := range []string{"over budget", root, "max_bytes=4096", "over_by=4096"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("warning missing %q; slog output:\n%s", want, logs.String())
		}
	}

	var kind, note string
	var scratchBytes, reapedBytes, reapedCount int
	queryRow(t, dbPath, `SELECT kind, scratch_bytes, scratch_reaped_bytes, scratch_reaped_count, note
		 FROM maintenance_runs`, &kind, &scratchBytes, &reapedBytes, &reapedCount, &note)
	if kind != "scratch-budget" || scratchBytes != 8192 || reapedBytes != 8192 || reapedCount != 1 {
		t.Errorf("row = (%q, %d, %d, %d), want (scratch-budget, 8192, 8192, 1)", kind, scratchBytes, reapedBytes, reapedCount)
	}
	if !strings.Contains(note, "over budget by 4096 bytes") {
		t.Errorf("note = %q, want it to carry the over-budget delta", note)
	}
}

// TestEnforceBudget_ReapIntoBudgetRecordsWithoutWarning: the reap brings the
// root back within budget, so nothing is warned, but the fired check still
// records its reaped counts.
func TestEnforceBudget_ReapIntoBudgetRecordsWithoutWarning(t *testing.T) {
	root, _ := budgetEnv(t, "4096", true)
	staleEntry(t, root, 8192)

	logs := captureLogs(t)
	res := EnforceBudget()

	if !res.Fired || res.OverBudget {
		t.Fatalf("want Fired and within budget after the reap: %+v", res)
	}
	if out := logs.String(); strings.Contains(out, "over budget") {
		t.Errorf("warning emitted although the reap restored budget:\n%s", out)
	}
	if !res.Recorded || res.ReapedCount != 1 {
		t.Errorf("recorded=%v reaped=%d, want recorded with 1 reaped entry", res.Recorded, res.ReapedCount)
	}
}

// TestEnforceBudget_ReapsAStaleReassessIDFile: a follow-up id file, 90 days old,
// over a one-byte budget. The id file's OWN window must reach it through the
// budget reap, and a fresh list survives even while the root is over budget.
func TestEnforceBudget_ReapsAStaleReassessIDFile(t *testing.T) {
	root, _ := budgetEnv(t, "1", false)
	stale := filepath.Join(root, "supersede-reassess-proj-20260101T000000Z-123-0123456789abcdef.ids")
	fresh := filepath.Join(root, "supersede-reassess-proj-20260102T000000Z-124-f00dbeefcafe1234.ids")
	for _, p := range []string{stale, fresh} {
		must(t, os.WriteFile(p, []byte(idFileBody), 0o600))
	}
	old := time.Now().Add(-90 * 24 * time.Hour)
	must(t, os.Chtimes(stale, old, old))

	logs := captureLogs(t)
	res := EnforceBudget()

	if !res.Fired || !res.OverBudget || res.ReapedCount != 1 {
		t.Errorf("want Fired, OverBudget and 1 reaped: %+v", res)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the stale id file survived the budget reap (stat err %v)", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("a fresh id file must survive even an over-budget reap: %v", err)
	}
	for _, want := range []string{"over budget", root, "reaped=1"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("warning missing %q; slog output:\n%s", want, logs.String())
		}
	}
}

// TestRecordBudgetRun_Notes: the recorded note is an audit trail, so it names a
// failed reap and the entries it could not remove, and never claims a
// successful reap after an error.
func TestRecordBudgetRun_Notes(t *testing.T) {
	tests := []struct {
		name    string
		res     BudgetResult
		want    []string
		notWant string
	}{
		{"reap error", BudgetResult{MaxBytes: 4096, ReapErr: errors.New("scratch root unreadable")},
			[]string{"reap", "failed"}, "within budget after reaping stale entries"},
		{"reap failed", BudgetResult{MaxBytes: 512, BytesAfter: 1024, ReapFailed: 2, OverBudget: true},
			[]string{"2 stale entries could not be removed"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, dbPath := budgetEnv(t, "4096", true)
			tt.res.Root = "/root/scratch"
			if !recordBudgetRun(tt.res) {
				t.Fatal("recordBudgetRun = false, want a recorded row")
			}
			note := noteOf(t, dbPath)
			for _, w := range tt.want {
				if !strings.Contains(note, w) {
					t.Errorf("note = %q, want it to contain %q", note, w)
				}
			}
			if tt.notWant != "" && strings.Contains(note, tt.notWant) {
				t.Errorf("note = %q, must not contain %q", note, tt.notWant)
			}
		})
	}
}

// TestSize_MeasuresWhatItCanWithUnreadableSubdir: an unreadable directory must
// not prevent Size from measuring the rest of the tree. Reproduction for issue
// #1960.
func TestSize_MeasuresWhatItCanWithUnreadableSubdir(t *testing.T) {
	root := t.TempDir()
	must(t, os.WriteFile(filepath.Join(root, "a.bin"), make([]byte, 1000), 0o600))
	sub := filepath.Join(root, "sub")
	must(t, os.Mkdir(sub, 0o700))
	must(t, os.WriteFile(filepath.Join(sub, "hidden.bin"), make([]byte, 2000), 0o600))
	lockDir(t, sub)

	logs := captureLogs(t)
	b, f, s, err := Size(root)
	if err != nil || b != 1000 || f != 1 || s != 1 {
		t.Errorf("Size = (%d, %d, %d, %v), want (1000, 1, 1, nil)", b, f, s, err)
	}
	if !strings.Contains(logs.String(), "skipped unreadable entries") {
		t.Errorf("skipped entries left no trace in the log:\n%s", logs.String())
	}
}

// TestEnforceBudget_MeasuresWhatItCanWithUnreadableSubdir: an unreadable subtree
// must not disable the budget check; the measurable portion over budget fires
// it and records a row. Reproduction for issue #1960.
func TestEnforceBudget_MeasuresWhatItCanWithUnreadableSubdir(t *testing.T) {
	root, _ := budgetEnv(t, "512", true)
	stale := staleEntry(t, root, 1024)
	blocked := filepath.Join(root, "blocked")
	must(t, os.Mkdir(blocked, 0o700))
	must(t, os.WriteFile(filepath.Join(blocked, "x"), []byte("x"), 0o600))
	lockDir(t, blocked)

	logs := captureLogs(t)
	res := EnforceBudget()

	if !res.Fired || !res.Recorded {
		t.Fatalf("budget check skipped because of an unreadable entry: %+v; logs:\n%s", res, logs.String())
	}
	if strings.Contains(logs.String(), "cannot measure root; skipping check") {
		t.Errorf("check was skipped on unreadable entry:\n%s", logs.String())
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale dir survived (stat err %v)", err)
	}
}

// TestSize_UnreadableRootReturnsError: an unreadable scratch root cannot be
// measured, so Size returns an error rather than a partial measurement.
func TestSize_UnreadableRootReturnsError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a mode-0o000 directory is still readable")
	}
	root := filepath.Join(t.TempDir(), "scratchroot")
	must(t, os.Mkdir(root, 0o700))
	must(t, os.WriteFile(filepath.Join(root, "a"), []byte("x"), 0o600))
	lockDir(t, root)

	if _, _, _, err := Size(root); err == nil {
		t.Fatal("Size returned no error for an unreadable root")
	}
}

// TestEnforceBudget_ReportsUnremovableStaleEntries: a stale entry whose removal
// fails makes the fired check report ReapFailed and record a row naming it.
func TestEnforceBudget_ReportsUnremovableStaleEntries(t *testing.T) {
	root, dbPath := budgetEnv(t, "512", true)
	stale := writeOwnerDir(t, root, "123-0123456789abcdef", "pid="+strconv.Itoa(1<<30)+"\ntoken=aa\nstart=gone\n")
	must(t, os.WriteFile(filepath.Join(stale, "payload"), make([]byte, 1024), 0o600))
	prev := removeTree
	removeTree = func(string) error { return errForcedRemoval }
	t.Cleanup(func() { removeTree = prev })

	logs := captureLogs(t)
	res := EnforceBudget()

	if !res.Fired || !res.Recorded || res.ReapedCount != 0 || res.ReapFailed != 1 {
		t.Fatalf("want fired and recorded with (reaped 0, failed 1): %+v", res)
	}
	for _, want := range []string{"could not remove stale entries", "failed=1"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log missing %q; slog output:\n%s", want, logs.String())
		}
	}
	if note := noteOf(t, dbPath); !strings.Contains(note, "1 stale entries could not be removed") {
		t.Errorf("note = %q, want it to name the unremoved stale entries", note)
	}
}

// TestEnforceBudget_PartialMeasurementFiresAndRecords: a locked subtree larger
// than the budget hides bytes nobody measured. A partial measurement must not
// be compared silently: the check fires, reaps through the lock, and records a
// row naming the skipped entries.
func TestEnforceBudget_PartialMeasurementFiresAndRecords(t *testing.T) {
	root, dbPath := budgetEnv(t, "4096", true)
	must(t, os.WriteFile(filepath.Join(root, "small"), []byte("small"), 0o600))
	// A dead-owner entry behind a lock: its marker cannot be read, so it is
	// reaped by the markerless age backstop.
	stale := staleEntry(t, root, 8192)
	lockDir(t, stale)

	logs := captureLogs(t)
	res := EnforceBudget()

	if !res.Fired || !res.Recorded || res.Skipped != 1 {
		t.Fatalf("want Fired, Recorded and Skipped=1: %+v", res)
	}
	if !strings.Contains(logs.String(), "skipped unreadable entries") {
		t.Errorf("warning missing the skipped count:\n%s", logs.String())
	}
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Errorf("locked stale entry survived the reap: %v", err)
	}
	if note := noteOf(t, dbPath); !strings.Contains(note, "measurement skipped") {
		t.Errorf("note = %q, want it to name the skipped entries", note)
	}
}

// TestEnforceBudget_SurvivingLockedEntryCountsOnce: a locked entry the reap
// leaves in place is skipped by both measures but is one unreadable subtree, so
// the recorded note says 1, not 2.
func TestEnforceBudget_SurvivingLockedEntryCountsOnce(t *testing.T) {
	root, dbPath := budgetEnv(t, "4096", true)
	// Fresh, so the reap's age backstop keeps it and it is still locked at the
	// re-measure.
	locked := filepath.Join(root, "123-0123456789abcdef")
	must(t, os.Mkdir(locked, 0o700))
	must(t, os.WriteFile(filepath.Join(locked, "payload"), make([]byte, 8192), 0o600))
	lockDir(t, locked)

	res := EnforceBudget()

	if _, err := os.Lstat(locked); err != nil {
		t.Fatalf("locked entry did not survive the reap: %v", err)
	}
	if !res.Fired || !res.Recorded || res.Skipped != 1 {
		t.Fatalf("want Fired, Recorded and Skipped=1: %+v", res)
	}
	if note := noteOf(t, dbPath); !strings.Contains(note, "measurement skipped 1 unreadable entries") {
		t.Errorf("note = %q, want it to say 1 skipped entry", note)
	}
}

// TestEnforceBudget_PartialMeasurementRecordsOncePerOutcome: an entry that stays
// unreadable under budget fires every spawn's check but adds one row per
// outcome, not per spawn, and the suppression is warned. A changed outcome
// (within budget, then over budget) gets a second row.
func TestEnforceBudget_PartialMeasurementRecordsOncePerOutcome(t *testing.T) {
	root, dbPath := budgetEnv(t, "1048576", true)
	small := filepath.Join(root, "small")
	must(t, os.WriteFile(small, []byte("x"), 0o600))
	locked := filepath.Join(root, "locked")
	must(t, os.Mkdir(locked, 0o700))
	lockDir(t, locked)

	logs := captureLogs(t)
	first := EnforceBudget()
	if !first.Fired || !first.Recorded || first.OverBudget {
		t.Fatalf("first run: want fired, recorded and within budget: %+v", first)
	}
	if second := EnforceBudget(); second.Recorded {
		t.Fatalf("suppressed outcome must not claim a row was written: %+v", second)
	}
	if !strings.Contains(logs.String(), "already recorded; suppressing repeated rows") {
		t.Errorf("suppression was not warned:\n%s", logs.String())
	}
	if n := rowCount(t, dbPath); n != 1 {
		t.Errorf("maintenance_runs rows = %d, want 1 (one per outcome, not per spawn)", n)
	}
	if note := noteOf(t, dbPath); !strings.Contains(note, "not recorded again") {
		t.Errorf("note = %q, want it to say repeats are not recorded again", note)
	}

	// Nothing is stale, so the reap cannot bring the root back under budget.
	must(t, os.WriteFile(small, make([]byte, 4096), 0o600))
	t.Setenv("GHOST_SCRATCH_MAX_BYTES", "1024")
	third := EnforceBudget()
	if !third.OverBudget || !third.Recorded {
		t.Fatalf("a changed partial outcome must be over budget and recorded: %+v", third)
	}
	if n := rowCount(t, dbPath); n != 2 {
		t.Errorf("maintenance_runs rows = %d, want 2 (within budget, then over budget)", n)
	}
}
