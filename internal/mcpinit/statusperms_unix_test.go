//go:build !windows

package mcpinit

import (
	"bytes"
	"testing"
)

// TestStatusLeavesTheDataDirectoryModesAlone pins the PERMISSION half of the
// status run's read-only promise, which is a separate promise from the
// migration one: the status run opens the store through memory.OpenReadDB, and
// that constructor runs neither the schema migration nor the permission pass,
// so the data directory and the database keep the modes they had.
//
// The modes are the ones an older build, a packaging script or a hand-run mkdir
// leaves behind — a data directory at 0750 and a database at 0640, which is the
// whole memory store readable by everyone in the user's group. The status run
// REPORTS on that store; it does not repair it. `ghost maintenance status` opens
// through memory.OpenDB and does tighten, so the two commands are not
// interchangeable for this, which is why the permissions documentation has to
// name them apart.
//
// Build-tagged because the assertions are about POSIX mode bits: Windows
// carries access in an ACL, memory.TightenPermissions is a deliberate no-op
// there, and the mode a Windows filesystem reports for a file the user owns is
// 0666 no matter what was asked for.
func TestStatusLeavesTheDataDirectoryModesAlone(t *testing.T) {
	statusEnv(t)
	dir, dbPath := looseDataDir(t)

	var out bytes.Buffer
	if _, err := Status(&out); err != nil {
		t.Fatalf("Status = %v, want a report rather than an error", err)
	}

	if got := permOf(t, dir); got != 0o750 {
		t.Errorf("data dir mode after a status run = %#o, want 0750 — the diagnostic tightened it", got)
	}
	if got := permOf(t, dbPath); got != 0o640 {
		t.Errorf("database mode after a status run = %#o, want 0640 — the diagnostic tightened it", got)
	}
}
