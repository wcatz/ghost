//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMaintenanceStatusMeasuresWhatItCanPastAnUnreadableScratchEntry: `ghost
// maintenance status` reports the scratch root's live usage. An unreadable
// entry inside the scratch root is skipped so the measurement reflects what it
// can (#1960); the root is not reported as unmeasurable and the command still
// exits 0.
func TestMaintenanceStatusMeasuresWhatItCanPastAnUnreadableScratchEntry(t *testing.T) {
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

	// The scratch root holds a readable file and a directory nobody can enter.
	// The unreadable subtree is skipped (#1960), so the header reports the bytes
	// it could measure rather than a failed measurement.
	const hostile = "locked\nname\x01"
	if err := os.WriteFile(filepath.Join(s.scratch, "readable"), []byte("small"), 0o600); err != nil {
		t.Fatalf("write the readable file: %v", err)
	}
	locked := filepath.Join(s.scratch, hostile)
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
	mustNotContain(t, "maintenance status with an unreadable scratch entry", r.stdout, "could not be measured")
	mustMatch(t, "maintenance status with an unreadable scratch entry", r.stdout, `5 bytes in 1 files`)
	mustContain(t, "maintenance status with an unreadable scratch entry", r.stdout, "(partial)")
}
