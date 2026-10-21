//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMaintenanceStatusReportsAnUnmeasurableScratchRoot: `ghost maintenance
// status` reports the scratch root's live usage, and a root it could NOT
// measure is not a root it measured as empty.
//
// scratch.Size returns (0, 0, err) when its walk meets an entry it cannot
// read, so the zeros that come back with the error are a FAILED measurement.
// The status line used to print them as the root's size — "0 bytes in 0
// files" beside a root that holds a readable file and a bigger one behind a
// lock — and the command exited 0, so nothing in its output said the
// measurement had failed. The header must name the failure instead.
//
// The exit code stays 0 and the test says so by using mustRun: the scratch
// side of this report is best-effort by the command's own contract ("an
// unusable root is reported in the header, not fatal"), and the budget check
// that measures the same root warns and continues rather than refusing the
// spawn. What must not happen is a report that states a size nobody measured.
func TestMaintenanceStatusReportsAnUnmeasurableScratchRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a mode-0o000 directory is still readable")
	}
	s := newSandbox(t)
	// A store, because the status command only measures the scratch root when
	// there is a database to report the hygiene runs from.
	call(t, s.mcpSession(t), "ghost_memory_save", map[string]any{
		"project_id": e2eProject,
		"content":    "a memory so the status command has a store to open",
	})

	// The scratch root holds a readable file and a directory nobody can
	// enter, so the walk fails on the second and the measurement is a failure
	// rather than a count.
	if err := os.WriteFile(filepath.Join(s.scratch, "readable"), []byte("small"), 0o600); err != nil {
		t.Fatalf("write the readable file: %v", err)
	}
	locked := filepath.Join(s.scratch, "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatalf("mkdir the locked directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(locked, "hidden"), []byte("a larger file behind the lock"), 0o600); err != nil {
		t.Fatalf("write the file behind the lock: %v", err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("lock the directory: %v", err)
	}
	// The sandbox's own cleanup has to be able to read the tree again.
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	r := s.mustRun("maintenance", "status")
	mustMatch(t, "maintenance status with an unreadable scratch entry", r.stdout, "(?i)could not be measured")
	mustNotContain(t, "maintenance status with an unreadable scratch entry", r.stdout, "0 bytes in 0 files")
}
