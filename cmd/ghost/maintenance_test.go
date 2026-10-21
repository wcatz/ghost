package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestPrintMaintenanceStatus_RunsAndScratchBytes: `ghost maintenance status`
// must show the live scratch bytes (root usage line) and, one line per run,
// each recorded event's scratch bytes, reaped counts, and timestamp.
func TestPrintMaintenanceStatus_RunsAndScratchBytes(t *testing.T) {
	runs := []memory.MaintenanceRun{
		{
			ID:                 "a",
			Kind:               "scratch-budget",
			RecordedAt:         "2026-09-24 12:00:00",
			ScratchBytes:       612345678,
			ScratchReapedBytes: 14155776,
			ScratchReapedCount: 3,
			Note:               "over budget by 75474766 bytes; proceeding",
		},
		{
			ID:           "b",
			Kind:         "scratch-budget",
			RecordedAt:   "2026-09-24 11:00:00",
			ScratchBytes: 512,
			Note:         "within budget after reaping stale entries",
		},
	}
	view := maintenanceStatusView{
		Root:      "/home/u/.local/share/ghost/scratch",
		RootBytes: 536870912,
		RootFiles: 12,
		Budget:    536870912,
		Runs:      runs,
	}

	var buf bytes.Buffer
	if err := printMaintenanceStatus(&buf, view); err != nil {
		t.Fatalf("printMaintenanceStatus: %v", err)
	}
	out := buf.String()

	// Live scratch bytes + root + budget on the header line.
	for _, want := range []string{
		"/home/u/.local/share/ghost/scratch",
		"536870912 bytes",
		"12 files",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status missing live-scratch %q:\n%s", want, out)
		}
	}
	// One line per run: timestamp, scratch bytes, reaped counts, note.
	for _, want := range []string{
		"2026-09-24 12:00:00",
		"612345678",
		"3",
		"14155776",
		"over budget by 75474766 bytes",
		"2026-09-24 11:00:00",
		"within budget after reaping stale entries",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}
	// Exactly one line per run under the header (no aggregation).
	if got := strings.Count(out, "scratch-budget"); got != 2 {
		t.Errorf("scratch-budget lines = %d, want 2 (one per run):\n%s", got, out)
	}
}

// TestPrintMaintenanceStatus_EmptyAndDisabledBudget: no runs yet and an
// explicit 0 budget (opt-out) both print human-readable states, not nothing.
func TestPrintMaintenanceStatus_EmptyAndDisabledBudget(t *testing.T) {
	view := maintenanceStatusView{
		Root:      "/root/scratch",
		RootBytes: 4096,
		RootFiles: 2,
		Budget:    0, // disabled
	}
	var buf bytes.Buffer
	if err := printMaintenanceStatus(&buf, view); err != nil {
		t.Fatalf("printMaintenanceStatus: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "disabled") {
		t.Errorf("budget=0 must print as disabled:\n%s", out)
	}
	if !strings.Contains(out, "no maintenance runs") {
		t.Errorf("empty runs must say so:\n%s", out)
	}
}

// TestPrintMaintenanceStatus_NegativeBudgetPrintsDisabled: EnforceBudget treats
// any non-positive budget as disabled (`maxBytes <= 0`), and ScratchConfig
// documents a negative value as behaving as 0. The status line must agree, or a
// `max_bytes: -1` prints as an active budget while enforcement is actually off.
func TestPrintMaintenanceStatus_NegativeBudgetPrintsDisabled(t *testing.T) {
	view := maintenanceStatusView{
		Root:      "/root/scratch",
		RootBytes: 4096,
		RootFiles: 2,
		Budget:    -1, // negative: documented as behaving as 0
	}
	var buf bytes.Buffer
	if err := printMaintenanceStatus(&buf, view); err != nil {
		t.Fatalf("printMaintenanceStatus: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "disabled") {
		t.Errorf("negative budget must print as disabled, matching EnforceBudget:\n%s", out)
	}
	if strings.Contains(out, "budget -1 bytes") {
		t.Errorf("negative budget printed as an active budget:\n%s", out)
	}
}

// TestPrintMaintenanceStatus_NoDatabase: a machine that has never run ghost's
// server has no DB — the status command reports that instead of failing.
func TestPrintMaintenanceStatus_NoDatabase(t *testing.T) {
	view := maintenanceStatusView{
		Root:       "/root/scratch",
		RootBytes:  0,
		Budget:     512 * 1024 * 1024,
		NoDatabase: true,
	}
	var buf bytes.Buffer
	if err := printMaintenanceStatus(&buf, view); err != nil {
		t.Fatalf("printMaintenanceStatus: %v", err)
	}
	if !strings.Contains(buf.String(), "no Ghost database") {
		t.Errorf("missing-DB state must be reported:\n%s", buf.String())
	}
}

// TestPrintMaintenanceStatus_UnmeasuredRootIsNotEmpty: a scratch root the
// command could not measure is not a scratch root it measured as empty.
// scratch.Size returns (0, 0, err) for a walk that met an entry it cannot
// read, so the zeros it returns with the error are a FAILED measurement;
// printing them as the root's size is what made the status line report an
// unmeasured root as an empty one.
func TestPrintMaintenanceStatus_UnmeasuredRootIsNotEmpty(t *testing.T) {
	view := maintenanceStatusView{
		Root:           "/root/scratch",
		RootMeasureErr: errors.New("size /root/scratch: permission denied"),
		Budget:         536870912,
	}
	var buf bytes.Buffer
	if err := printMaintenanceStatus(&buf, view); err != nil {
		t.Fatalf("printMaintenanceStatus: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "0 bytes in 0 files") {
		t.Errorf("an unmeasured root must not be reported as an empty one:\n%s", out)
	}
	if !strings.Contains(out, "could not be measured") {
		t.Errorf("an unmeasured root must say so:\n%s", out)
	}
	if !strings.Contains(out, "permission denied") {
		t.Errorf("an unmeasured root must name why it could not be measured:\n%s", out)
	}
	// The budget is a separate fact and is still reported: the measurement
	// failed, not the configuration.
	if !strings.Contains(out, "budget 536870912 bytes") {
		t.Errorf("the budget must still be reported beside a failed measurement:\n%s", out)
	}
}

// TestMeasureScratchRootReportsAFailedWalk: the live measurement carries the
// failure with it, because scratch.Size returns (0, 0, err) when its walk
// meets an entry it cannot read — the zeros are a failed measurement, not an
// empty root. The root's path is still reported, so the operator knows WHICH
// root could not be measured.
func TestMeasureScratchRootReportsAFailedWalk(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a mode-0o000 directory is still readable")
	}
	root := t.TempDir()
	locked := filepath.Join(root, "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatalf("mkdir the locked directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(locked, "hidden"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write the file behind the lock: %v", err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("lock the directory: %v", err)
	}
	// The temp dir's own cleanup has to be able to read the tree again.
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	t.Setenv("GHOST_SCRATCH_DIR", root)

	view := maintenanceStatusView{Budget: 1}
	measureScratchRoot(&view)
	if view.RootMeasureErr == nil {
		t.Fatalf("the failed walk was reported as a measurement of %d bytes in %d files",
			view.RootBytes, view.RootFiles)
	}
	if view.Root != root {
		t.Errorf("root = %q, want %q — the operator has to know which root failed", view.Root, root)
	}
}

// TestMeasureScratchRootReportsAMeasuredRoot: the ordinary case still reports
// the counts, so the failure branch above cannot pass by never measuring
// anything.
func TestMeasureScratchRootReportsAMeasuredRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "readable"), []byte("small"), 0o600); err != nil {
		t.Fatalf("write the readable file: %v", err)
	}
	t.Setenv("GHOST_SCRATCH_DIR", root)

	view := maintenanceStatusView{Budget: 1}
	measureScratchRoot(&view)
	if view.RootMeasureErr != nil {
		t.Fatalf("a readable root measured as a failure: %v", view.RootMeasureErr)
	}
	if view.Root != root {
		t.Errorf("root = %q, want %q", view.Root, root)
	}
	if view.RootBytes != 5 || view.RootFiles != 1 {
		t.Errorf("measured %d bytes in %d files, want 5 bytes in 1 file", view.RootBytes, view.RootFiles)
	}
}

// TestMeasureScratchRootReportsAnUnresolvableRoot: a root that cannot even
// be resolved is the same state one step earlier — there is nothing to
// measure, so the reason is reported rather than a size nobody measured.
func TestMeasureScratchRootReportsAnUnresolvableRoot(t *testing.T) {
	// A path under a regular file cannot be a directory, so resolving the root
	// fails without touching anything real.
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write the file: %v", err)
	}
	t.Setenv("GHOST_SCRATCH_DIR", filepath.Join(file, "scratch"))

	view := maintenanceStatusView{Budget: 1}
	measureScratchRoot(&view)
	if view.RootMeasureErr == nil {
		t.Fatalf("an unresolvable root was reported as a measurement of %d bytes in %d files",
			view.RootBytes, view.RootFiles)
	}
	if view.Root != "(unavailable)" {
		t.Errorf("root = %q, want %q", view.Root, "(unavailable)")
	}

	var buf bytes.Buffer
	if err := printMaintenanceStatus(&buf, view); err != nil {
		t.Fatalf("printMaintenanceStatus: %v", err)
	}
	if strings.Contains(buf.String(), "0 bytes in 0 files") {
		t.Errorf("an unresolvable root must not be reported as an empty one:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "could not be measured") {
		t.Errorf("an unresolvable root must say so:\n%s", buf.String())
	}
}

// TestParseMaintenanceArgs: status takes no flags; clean-scratch takes only
// --apply; anything else is an error rather than being silently ignored (a
// silently misparsed flag could turn a report into a removal or vice versa).
func TestParseMaintenanceArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		apply   bool
		wantErr bool
	}{
		{"no args to clean-scratch", nil, false, false},
		{"apply", []string{"--apply"}, true, false},
		{"unknown flag", []string{"--force"}, false, true},
		{"positional junk", []string{"everything"}, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			apply, err := parseCleanScratchArgs(tt.args)
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if apply != tt.apply {
				t.Errorf("apply = %v, want %v", apply, tt.apply)
			}
		})
	}
}
