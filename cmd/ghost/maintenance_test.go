package main

import (
	"bytes"
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
