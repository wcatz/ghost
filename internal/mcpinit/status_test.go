package mcpinit

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStatus_ReportsOpenDBFailure verifies that a database which exists but
// fails to open (e.g. mid-migration foreign-key corruption) is surfaced as a
// failed check, not silently skipped. Before this fix, Status only inspected
// the database when memory.OpenDB succeeded, so a broken database looked
// identical to "All checks passed."
func TestStatus_ReportsOpenDBFailure(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataDir)
	ghostDir := filepath.Join(dataDir, "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir ghost dir: %v", err)
	}
	dbPath := filepath.Join(ghostDir, "ghost.db")
	if err := os.WriteFile(dbPath, []byte("not a sqlite database"), 0o600); err != nil {
		t.Fatalf("write fake db: %v", err)
	}

	// Isolate PATH so Status can't shell out to a host-installed `claude`
	// binary — this test only exercises the database-open-failure check.
	t.Setenv("PATH", t.TempDir())

	var out bytes.Buffer
	if err := Status(&out); err != nil {
		t.Fatalf("Status: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "✗ database:") {
		t.Errorf("expected a failed database check, got:\n%s", output)
	}
	if strings.Contains(output, "All checks passed.") {
		t.Errorf("a broken database must not report \"All checks passed.\", got:\n%s", output)
	}
}
