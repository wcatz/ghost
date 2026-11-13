//go:build linux

package mcpinit

// The rendered report, as the operator reads it.
//
// The scan is only worth having if its output is acted on, and that depends on
// two things a caller cannot see from the data: that a HEALTHY machine prints
// nothing at all, and that a stale line names the client to restart rather than
// only a pid. Both are pinned here, because both fail silently — a permanent
// section gets skimmed, and a bare pid gets ignored.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReportStaleServersNamesTheClientAndIsSilentWhenClean(t *testing.T) {
	dir := t.TempDir()
	installed := filepath.Join(dir, "ghost")
	if err := os.WriteFile(installed, []byte("current build"), 0o755); err != nil { //nolint:gosec
		t.Fatalf("write installed binary: %v", err)
	}
	root := filepath.Join(dir, "proc")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir proc root: %v", err)
	}

	// Healthy: a current server and its client. Nothing may be printed.
	writeFakeProc(t, root,
		procEntry{pid: 100, comm: "ghost", cmdline: []string{installed, "mcp"}, ppid: 42, exeLink: installed},
		procEntry{pid: 42, comm: "claude", cmdline: []string{"claude"}, ppid: 1, exeLink: "/usr/bin/node"},
	)
	var healthy strings.Builder
	if n := ReportStaleServersTo(&healthy, root, installed); n != 0 {
		t.Errorf("reported %d stale servers on a healthy tree, want 0: %q", n, healthy.String())
	}
	if healthy.Len() != 0 {
		t.Errorf("printed %q on a healthy tree, want nothing", healthy.String())
	}

	// Now replace the binary underneath it, as `ghost upgrade` does.
	writeFakeProc(t, root,
		procEntry{pid: 100, comm: "ghost", cmdline: []string{installed, "mcp"}, ppid: 42, exeLink: installed, exeDeleted: true},
	)
	var stale strings.Builder
	if n := ReportStaleServersTo(&stale, root, installed); n != 1 {
		t.Errorf("reported %d stale servers after the binary was replaced, want 1", n)
	}
	out := stale.String()
	// The pid, so the operator can find the process.
	if !strings.Contains(out, "100") {
		t.Errorf("report does not name the pid:\n%s", out)
	}
	// The CLIENT, which is the actionable part: "restart pid 100" is not an
	// instruction, "restart the claude session" is.
	if !strings.Contains(out, "claude") {
		t.Errorf("report does not name the parent client to restart:\n%s", out)
	}
	// The reason, so the operator knows this is not a stale pid file.
	if !strings.Contains(out, "deleted") {
		t.Errorf("report does not say the executable was deleted:\n%s", out)
	}
	// The remedy.
	if !strings.Contains(out, "restart") {
		t.Errorf("report does not say what to do:\n%s", out)
	}
	// The consequence, hedged. A server on a replaced binary refuses its writes
	// only once the store has moved past it — which this code never opens, and
	// so cannot know. It says "may be refused" rather than asserting a store
	// state it did not read, and the test pins that difference because an
	// unhedged claim here would be a statement about a database this code has
	// not touched.
	if !strings.Contains(out, "may be refused") {
		t.Errorf("report does not hedge the consequence it cannot verify:\n%s", out)
	}
}
