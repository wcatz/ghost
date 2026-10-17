//go:build e2e && !windows

package e2e

// The lifecycle coordinator's own signal handling. Each phase runs in a process
// group of its own so a deadline can sweep the phase and the CLI harness it
// spawned, which means a SIGINT/SIGTERM delivered to the coordinator alone — a
// supervisor stopping that pid, or `kill <pid>` — reaches neither. Without a
// handler the coordinator dies on Go's default action: the phase keeps running
// and writing in a group nothing signalled, the per-project claim's pid file
// names a dead pid, and no failure marker is recorded. This file drives the
// built binary through exactly that, against a phase that is blocked on its
// harness when the signal lands.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/mcpinit"
)

// TestLifecycleSignalStopsTheRunningPhase signals the coordinator's own pid
// while a phase is blocked on a slow harness and requires two things of the
// interruption: the phase and its harness are terminated (not orphaned), and
// the interrupted phase is recorded like a failed one, so the next session
// start raises the maintenance alert rather than silence.
func TestLifecycleSignalStopsTheRunningPhase(t *testing.T) {
	s := newSandbox(t)
	cs := s.mcpSession(t)
	// Content carrying a resolution keyword ("reverted"), so the resolve phase
	// has a candidate and really blocks on its harness.
	call(t, cs, "ghost_memory_save", map[string]any{
		"project_id": e2eProject,
		"content":    "the failed migration was reverted on the staging relay",
	})
	s.mustRun("project", "bind", e2eProject, s.work)
	s.reconfigure(configOpts{autoResolve: true, minInterval: "0"})

	// Make the resolve phase's harness call slow, so the phase is still running
	// when the signal lands. The fake records the pid it is sleeping as, which is
	// the process the phase's own signal handling has to reach.
	if err := os.WriteFile(filepath.Join(s.bin, "resolve.delay"), []byte("120\n"), 0o644); err != nil {
		t.Fatalf("write the harness delay: %v", err)
	}
	pidFile := filepath.Join(s.bin, "resolve.pid")

	cmd := exec.Command(ghostBin, "lifecycle", "--project", e2eProject)
	cmd.Env = s.env
	cmd.Dir = s.work
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Start(); err != nil {
		t.Fatalf("start ghost lifecycle: %v", err)
	}
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		_ = cmd.Wait()
	}()
	t.Cleanup(func() {
		select {
		case <-exited:
		default:
			_ = cmd.Process.Kill()
			<-exited
		}
		// The phase is the coordinator's child in a group of its own, so a
		// coordinator killed without terminating it leaves an orphan this
		// cleanup has to end rather than leak. The harness pid is the group
		// leader the sandbox recorded.
		if pid := pidInFile(pidFile); pid > 0 && pidAlive(pid) {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})

	// Wait until the phase is actually blocked on its harness, so the signal
	// lands mid-phase rather than during startup.
	waitForFile(t, pidFile, 60*time.Second, "the resolve phase's harness call")
	harnessPid := readPidFile(t, pidFile)

	// Signal the coordinator ALONE, the way `kill <pid>` does: the phase is in
	// its own group and receives nothing directly.
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal the coordinator: %v", err)
	}
	select {
	case <-exited:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		<-exited
		t.Fatalf("ghost lifecycle did not exit after SIGINT\n%s", errb.String())
	}

	// The interrupted run is recorded: a failed phase leaves the per-project
	// failure marker the next session start turns into an alert.
	marker := filepath.Join(s.dataDir(), mcpinit.LifecycleFailureMarkerFile(e2eProject))
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("no failure marker after the signal, so the interrupted run went unrecorded: %v\n%s", err, errb.String())
	}
	var m struct {
		PhasesFailed []string `json:"phases_failed"`
		Error        string   `json:"error"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal the marker %q: %v", b, err)
	}
	if !containsString(m.PhasesFailed, "resolve") {
		t.Errorf("marker phases_failed = %v, want it to name the interrupted resolve phase (error: %q)", m.PhasesFailed, m.Error)
	}

	// And the phase's group was signalled rather than orphaned: the harness the
	// phase was blocked on is gone.
	if err := waitPidGone(harnessPid, 15*time.Second); err != nil {
		t.Errorf("the phase's harness (pid %d) survived the signal — the phase was orphaned, not terminated: %v\n%s", harnessPid, err, errb.String())
	}
}

// readPidFile reads a pid a sandbox helper recorded, failing when it cannot.
func readPidFile(t *testing.T, path string) int {
	t.Helper()
	pid := pidInFile(path)
	if pid <= 0 {
		t.Fatalf("no pid in %s", path)
	}
	return pid
}

// pidInFile reads a pid a sandbox helper recorded, or 0 when the file is absent
// or unreadable. The cleanup path uses this form: a test that aborted before the
// helper wrote anything still has to sweep, and a missing file is not a crash.
func pidInFile(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0
	}
	return pid
}

// pidAlive reports whether pid still exists. EPERM means it exists but is not
// ours to signal, which still counts as alive. A pid of zero or below names no
// process at all.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// waitPidGone polls until pid is gone, so the test does not race a process that
// was signalled a moment ago still showing up as a zombie.
func waitPidGone(pid int, within time.Duration) error {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("still running after %s", within)
}
