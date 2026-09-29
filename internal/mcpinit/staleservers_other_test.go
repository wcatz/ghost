//go:build !linux

package mcpinit

// The off-Linux contract: the scan reports itself UNANSWERED rather than
// returning an empty list.
//
// Those two answers look identical to a caller that ignores the boolean, and
// only one of them is true. An empty list says "no stale servers are running",
// which on macOS or Windows is a claim nobody checked — and an operator who
// believed it would conclude their install was healthy on a machine where the
// question was never asked.
//
// The fixture tree is deliberately a real one: this test is about the refusal to
// answer, so the input has to be readable and the answer must still be "not
// checked".

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStaleServersReportsItselfUncheckedOffLinux(t *testing.T) {
	dir := t.TempDir()
	installed := filepath.Join(dir, "ghost")
	if err := os.WriteFile(installed, []byte("current build"), 0o755); err != nil { //nolint:gosec
		t.Fatalf("write installed binary: %v", err)
	}
	pidDir := filepath.Join(dir, "proc", "100")
	if err := os.MkdirAll(pidDir, 0o755); err != nil {
		t.Fatalf("mkdir proc entry: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pidDir, "comm"), []byte("ghost\n"), 0o644); err != nil { //nolint:gosec
		t.Fatalf("write comm: %v", err)
	}

	found, checked := StaleGhostServers(filepath.Join(dir, "proc"), installed)
	if checked {
		t.Error("checked = true off Linux, but there is no portable way to read a process's executable")
	}
	if len(found) != 0 {
		t.Errorf("found %+v, want nothing — an unanswerable scan must not look like a clean one", found)
	}
}

// The report stays silent when it cannot answer, for the same reason: a section
// headed by a diagnostic that did not run is worse than no section.
func TestReportStaleServersIsSilentWhenUnchecked(t *testing.T) {
	var sb strings.Builder
	ReportStaleServers(&sb, "/usr/local/bin/ghost")
	if sb.Len() != 0 {
		t.Errorf("ReportStaleServers wrote %q off Linux, want nothing", sb.String())
	}
}
