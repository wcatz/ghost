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

// The wording must not claim more than the evidence carries.
//
// `installed` is the path of whatever binary the operator happened to invoke,
// NOT a discovered installation: `go run ./cmd/ghost mcp status`, a `make build`
// output and a ~/bin copy are all ordinary ways to run this command, and a dev
// running one of them has perfectly current clients. Only ExeDeleted is a fact
// about the world; a plain mismatch says nothing more than "not this file".
// Saying "is no longer installed" turns a dev's dev-build invocation into
// "3 stale servers" and a scripted restart of working sessions, so the report
// names what it COMPARED.
func TestTheReportDoesNotClaimTheBinaryIsNoLongerInstalled(t *testing.T) {
	dir := t.TempDir()
	// The "installed" binary is a dev build at an arbitrary path, which is the
	// whole point: nothing discovered an installation here.
	devBuild := filepath.Join(dir, "go-build-ghost")
	if err := os.WriteFile(devBuild, []byte("dev build"), 0o755); err != nil { //nolint:gosec
		t.Fatalf("write dev build: %v", err)
	}
	// What the clients are actually running.
	released := filepath.Join(dir, "usr-local-bin-ghost")
	if err := os.WriteFile(released, []byte("released"), 0o755); err != nil { //nolint:gosec
		t.Fatalf("write released binary: %v", err)
	}
	root := filepath.Join(dir, "proc")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir proc root: %v", err)
	}
	writeFakeProc(t, root,
		procEntry{pid: 300, comm: "ghost", cmdline: []string{released, "mcp"}, ppid: 42, exeLink: released},
		procEntry{pid: 42, comm: "claude", cmdline: []string{"claude"}, ppid: 1, exeLink: "/usr/bin/node"},
	)

	var out strings.Builder
	if n := ReportStaleServersTo(&out, root, devBuild); n != 1 {
		t.Fatalf("reported %d, want 1 — the server does run a different binary, and that is a fact:\n%s", n, out.String())
	}
	got := out.String()
	// Every phrase that turns "not this file" into a claim about an
	// installation, or about what happened to the file.
	for _, overclaim := range []string{
		"is no longer installed",
		"not the installed",
		"the installed ghost",
	} {
		if strings.Contains(got, overclaim) {
			t.Errorf("report claims %q, which asserts an installation this code never discovered:\n%s", overclaim, got)
		}
	}
	// And it must still say what it DID compare, or the operator cannot act.
	if !strings.Contains(got, "different ghost binary than this one") {
		t.Errorf("report does not say what was compared:\n%s", got)
	}
	// The remedy points at THIS ghost rather than at an installation.
	if !strings.Contains(got, "so they start this ghost") {
		t.Errorf("remedy still tells the operator to start an installation rather than this binary:\n%s", got)
	}
}

// A server whose executable this user cannot stat is UNKNOWN, not different.
//
// The reachable case is a system-wide install: a `ghost mcp` owned by another
// user, running from a root-owned /opt/ghost at 0700, whose /proc/<pid>/exe
// link this user may not resolve. os.Stat then fails, and a two-valued
// sameFile returned false — which the one caller read as "a different file" and
// the report rendered as "restart this healthy client". The scan must skip it:
// not reported is a small loss, wrongly reported costs an operator a session.
func TestAServerWhoseExecutableCannotBeReadIsNotCalledStale(t *testing.T) {
	dir := t.TempDir()
	installed := filepath.Join(dir, "ghost")
	if err := os.WriteFile(installed, []byte("current build"), 0o755); err != nil { //nolint:gosec
		t.Fatalf("write installed binary: %v", err)
	}
	// A path that does not exist and cannot be resolved, standing in for one
	// this user has no permission to stat. It is a path with no file behind it,
	// which is exactly what os.Stat reports for an unreadable /proc/<pid>/exe.
	unreadable := filepath.Join(dir, "opt-ghost-another-user")
	root := filepath.Join(dir, "proc")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir proc root: %v", err)
	}
	writeFakeProc(t, root,
		procEntry{pid: 400, comm: "ghost", cmdline: []string{unreadable, "mcp"}, ppid: 42, exeLink: unreadable},
		procEntry{pid: 42, comm: "claude", cmdline: []string{"claude"}, ppid: 1, exeLink: "/usr/bin/node"},
	)

	var out strings.Builder
	if n := ReportStaleServersTo(&out, root, installed); n != 0 {
		t.Errorf("reported %d stale servers, want 0 — the executable could not be read, which is not evidence "+
			"of a mismatch:\n%s", n, out.String())
	}
	if out.Len() != 0 {
		t.Errorf("printed a report about a process it could not compare:\n%s", out.String())
	}
}
