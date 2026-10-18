//go:build e2e && !windows

package e2e

// The lifecycle coordinator's own signal handling. Each phase runs in a process
// group of its own so a deadline can sweep the phase and the CLI harness it
// spawned, which means a SIGINT/SIGTERM/SIGHUP delivered to the coordinator alone — a
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
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/mcpinit"
)

// syncBuffer is a bytes.Buffer whose writes are safe alongside reads. os/exec
// copies a child's output into the writer from its own goroutine, and only
// cmd.Wait — which here runs in a separate goroutine signalled by 'exited' —
// guarantees that copy has finished. The wait loop below reads the buffer on
// its failure paths while the coordinator is still running and the copier is
// still writing, which `go test -race` flags as a data race.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestLifecycleSignalStopsTheRunningPhase signals the coordinator's own pid
// while a phase is blocked on a slow harness and requires three things of the
// interruption: the coordinator WAITS for the phase rather than abandoning it
// (while the harness is alive the coordinator, and the per-project claim it
// holds, must be too), the phase's harness is terminated rather than orphaned,
// and the interrupted phase is recorded, so the next session start raises the
// maintenance alert rather than silence.
func TestLifecycleSignalStopsTheRunningPhase(t *testing.T) {
	lifecycleSignalStopsTheRunningPhase(t, os.Interrupt)
}

// TestLifecycleSIGHUPStopsTheRunningPhase is the same interruption by a hangup:
// closing the terminal or losing the SSH connection a foreground `ghost
// lifecycle` runs under. Without a handler for it the coordinator dies on Go's
// default action and leaves the phase's harness running, the claim naming a
// dead pid and no marker, exactly as SIGINT and SIGTERM once did.
func TestLifecycleSIGHUPStopsTheRunningPhase(t *testing.T) {
	lifecycleSignalStopsTheRunningPhase(t, syscall.SIGHUP)
}

// lifecycleSignalStopsTheRunningPhase is the body of the signal tests above:
// it delivers sig to the coordinator alone while a phase is blocked on a slow
// harness, and asserts the phase is waited for and terminated, the claim is
// released and the interruption is recorded.
func lifecycleSignalStopsTheRunningPhase(t *testing.T, sig os.Signal) {
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

	// Make the resolve phase's harness call slow — and make the harness keep
	// running for a moment after it is asked to stop — so the phase is still
	// running when the signal lands, and stays running long enough to see that
	// the coordinator waited for it. The fake records the pid it is sleeping
	// as, which is the process the phase's own signal handling has to reach.
	if err := os.WriteFile(filepath.Join(s.bin, "resolve.delay"), []byte("120\n"), 0o644); err != nil {
		t.Fatalf("write the harness delay: %v", err)
	}
	// Long enough to observe, and shorter than the harness call's own grace
	// (internal/ai's harnessWaitDelay, two seconds): a harness that outlived
	// that grace would be given up on by the phase itself, and the coordinator's
	// exit would no longer be the thing that ends it.
	if err := os.WriteFile(filepath.Join(s.bin, "resolve.trapdelay"), []byte("1\n"), 0o644); err != nil {
		t.Fatalf("write the harness trap delay: %v", err)
	}
	pidFile := filepath.Join(s.bin, "resolve.pid")

	// --source is explicit rather than left to the coordinator's phase
	// self-detection: the sandbox env carries no harness marker and the phase's
	// ancestor chain is the test runner, so an undetectable source would make
	// the resolve phase fail fast before it reaches its harness — and the run
	// would never be blocked on the call this test signals.
	cmd := exec.Command(ghostBin, "lifecycle", "--project", e2eProject, "--source", "claude-code")
	cmd.Env = s.env
	cmd.Dir = s.work
	var out, errb syncBuffer
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

	// The per-project claim the coordinator holds for the whole run: the lock a
	// second chain is turned away by. It names a LIVE pid for as long as the
	// phase it was claimed for is running, and is gone once the coordinator is.
	lockPath := filepath.Join(s.dataDir(), "lifecycle-"+e2eProject+".pid")
	waitForFile(t, lockPath, 60*time.Second, "the coordinator's per-project claim")
	coordPid := cmd.Process.Pid

	// Signal the coordinator ALONE, the way `kill <pid>` does: the phase is in
	// its own group and receives nothing directly.
	if err := cmd.Process.Signal(sig); err != nil {
		t.Fatalf("signal the coordinator: %v", err)
	}

	// The coordinator must WAIT for the phase rather than return the instant the
	// signal lands. The phase is blocked on a harness that traps the SIGTERM and
	// stays up for resolve.trapdelay, so for as long as that harness is alive the
	// coordinator — and the claim it holds — must be too. A coordinator that
	// returned on the signal would leave the phase and its harness running beside
	// a released lock, which is the interruption this test exists to get right.
	deadline := time.Now().Add(20 * time.Second)
	for pidAlive(harnessPid) && time.Now().Before(deadline) {
		if !pidAlive(coordPid) {
			t.Fatalf("the coordinator exited while the phase's harness (pid %d) was still running: the phase was abandoned, not waited for\n%s", harnessPid, errb.String())
		}
		if pid := pidInFile(lockPath); !pidAlive(pid) {
			t.Fatalf("the per-project claim named pid %d, not a live pid, while the phase's harness (pid %d) was still running: the lock was released before the phase ended\n%s", pid, harnessPid, errb.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pidAlive(harnessPid) {
		t.Fatalf("the phase's harness (pid %d) was still running after %s: the coordinator never terminated the phase it was waiting on\n%s", harnessPid, 20*time.Second, errb.String())
	}

	select {
	case <-exited:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		<-exited
		t.Fatalf("ghost lifecycle did not exit after %v\n%s", sig, errb.String())
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

	// The claim is free once the coordinator is gone, so the next chain can run.
	waitForFileGone(t, lockPath, 15*time.Second, "the per-project claim to be released")
}

// TestLifecycleIgnoredSIGHUPKeepsTheRunGoing starts the coordinator with SIGHUP
// already ignored, the way `nohup ghost lifecycle ...` does, and hangs it up
// mid-phase. A run started under nohup asked to outlive its terminal, so the
// hangup must neither cancel the phase nor end the coordinator: catching
// SIGHUP unconditionally would replace the inherited ignore with a handler and
// turn the hangup nohup exists to absorb into an interruption.
func TestLifecycleIgnoredSIGHUPKeepsTheRunGoing(t *testing.T) {
	s := newSandbox(t)
	cs := s.mcpSession(t)
	call(t, cs, "ghost_memory_save", map[string]any{
		"project_id": e2eProject,
		"content":    "the failed migration was reverted on the staging relay",
	})
	s.mustRun("project", "bind", e2eProject, s.work)
	s.reconfigure(configOpts{autoResolve: true, minInterval: "0"})
	if err := os.WriteFile(filepath.Join(s.bin, "resolve.delay"), []byte("120\n"), 0o644); err != nil {
		t.Fatalf("write the harness delay: %v", err)
	}
	pidFile := filepath.Join(s.bin, "resolve.pid")

	// The shell sets the ignore and then execs the coordinator in its own
	// place, so the pid signalled below is the coordinator's and it starts with
	// SIGHUP ignored, exactly as under nohup.
	cmd := exec.Command("/bin/sh", "-c", `trap "" HUP; exec "$0" "$@"`,
		ghostBin, "lifecycle", "--project", e2eProject, "--source", "claude-code")
	cmd.Env = s.env
	cmd.Dir = s.work
	var out, errb syncBuffer
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
		if pid := pidInFile(pidFile); pid > 0 && pidAlive(pid) {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})

	waitForFile(t, pidFile, 60*time.Second, "the resolve phase's harness call")
	harnessPid := readPidFile(t, pidFile)
	lockPath := filepath.Join(s.dataDir(), "lifecycle-"+e2eProject+".pid")
	waitForFile(t, lockPath, 60*time.Second, "the coordinator's per-project claim")

	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatalf("signal the coordinator: %v", err)
	}
	// Long enough for a coordinator that took the hangup to cancel the phase
	// and for the harness to take the group SIGTERM, which the fake answers at
	// once when no trap delay is set.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			t.Fatalf("the coordinator exited on a SIGHUP it started out ignoring\n%s", errb.String())
		default:
		}
		if !pidAlive(harnessPid) {
			t.Fatalf("the phase's harness (pid %d) ended on a SIGHUP the coordinator started out ignoring\n%s", harnessPid, errb.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pid := pidInFile(lockPath); pid != cmd.Process.Pid {
		t.Errorf("the per-project claim names pid %d, want the still-running coordinator %d", pid, cmd.Process.Pid)
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
//
// The value is cut at the first ":" because two formats reach here. The fake
// harness records a bare pid ("$$"), while the per-project lifecycle claim is
// written by mcpinit as "<pid>:<creation-token>" — the token is how the claim
// tells a reused pid apart from a live one — and only the pid before the colon
// is the process to look up.
func pidInFile(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pidStr, _, _ := strings.Cut(strings.TrimSpace(string(raw)), ":")
	pid, err := strconv.Atoi(pidStr)
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
