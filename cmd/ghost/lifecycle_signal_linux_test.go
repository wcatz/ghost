//go:build linux

package main

// The between-phases half of the coordinator's interrupt tests. It is Linux
// only because the way it parks the coordinator is: it reads the pipe's size
// with F_GETPIPE_SZ and finds the coordinator's blocked write in
// /proc/<pid>/task/<tid>/syscall. The sandbox, the fake harness and the
// coordinator helpers are in lifecycle_signal_unix_test.go.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/wcatz/ghost/internal/mcpinit"
)

// signalCancelMargin is how long the between-phases test keeps the coordinator
// parked on its stderr after the signal has left the pending set; see the
// comment where it is used.
const signalCancelMargin = 250 * time.Millisecond

// stderrParkHeadroom is the room the between-phases test leaves in the
// coordinator's stderr pipe: enough for the run's first line and too little
// for its first two. Every coordinator line is lifecycleLine's shape, a 20-byte
// time and `lifecycle: project "sigproj": ` before the message, so the first,
// `run started pid=<pid>`, is at most 78 bytes, and any two together are more
// than 120.
const stderrParkHeadroom = 100

// signalPending reports whether sig is still pending for pid, from the
// SigPnd and ShdPnd masks in /proc/<pid>/status. ok is false where /proc is
// not there to ask.
func signalPending(pid int, sig syscall.Signal) (pending, ok bool) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return false, false
	}
	bit := uint64(1) << (uint(sig) - 1)
	for _, line := range strings.Split(string(raw), "\n") {
		name, val, found := strings.Cut(line, ":")
		if !found || (name != "SigPnd" && name != "ShdPnd") {
			continue
		}
		mask, err := strconv.ParseUint(strings.TrimSpace(val), 16, 64)
		if err == nil && mask&bit != 0 {
			return true, true
		}
	}
	return false, true
}

// parkingPipe returns a pipe whose buffer is full but for stderrParkHeadroom
// bytes, and how many bytes the test put in it. Both ends are blocking, so a
// process writing to w blocks in write(2) once the headroom is spent: a pipe
// write that does not fit the room left in the last page waits for a free
// page, and this pipe has none until the reader drains it.
func parkingPipe(t *testing.T) (r, w *os.File, prefill int) {
	t.Helper()
	var fds [2]int
	if err := syscall.Pipe2(fds[:], syscall.O_CLOEXEC); err != nil {
		t.Fatalf("pipe2: %v", err)
	}
	r, w = os.NewFile(uintptr(fds[0]), "stderr-r"), os.NewFile(uintptr(fds[1]), "stderr-w")
	t.Cleanup(func() {
		_ = r.Close()
		_ = w.Close()
	})
	size, err := unix.FcntlInt(uintptr(fds[1]), unix.F_GETPIPE_SZ, 0)
	if err != nil {
		t.Fatalf("F_GETPIPE_SZ: %v", err)
	}
	prefill = size - stderrParkHeadroom
	if _, err := w.Write(bytes.Repeat([]byte{'.'}, prefill)); err != nil {
		t.Fatalf("fill the stderr pipe: %v", err)
	}
	return r, w, prefill
}

// blockedOnStderr reports whether a thread of pid is blocked in a write to
// file descriptor 2, from /proc/<pid>/task/<tid>/syscall, which reads
// "<number> <arg0> ..." for a thread in a system call. ok is false when the
// file cannot be read for a reason other than a thread that has gone.
func blockedOnStderr(pid int) (blocked, ok bool) {
	tasks, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	if err != nil {
		return false, false
	}
	write := strconv.Itoa(syscall.SYS_WRITE)
	for _, task := range tasks {
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%s/syscall", pid, task.Name()))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
				continue
			}
			return false, false
		}
		f := strings.Fields(string(raw))
		if len(f) >= 2 && f[0] == write && f[1] == "0x2" {
			return true, true
		}
	}
	return false, true
}

// TestLifecycleInterruptBetweenPhasesStartsNoPhase holds the coordinator in
// the gap before its first phase and signals it there. Its stderr is a pipe
// the test has filled to within stderrParkHeadroom of its size, so the run's
// first line, `run started`, fits and its second does not: the coordinator
// blocks in that write. Every line after the first is written after the signal
// handler is installed (the lock's own lines aside, and this run takes the
// lock), so a coordinator blocked on its stderr past the first line is one a
// SIGINT interrupts rather than kills, and one that has not reached its phase
// loop. The pipe is drained only after the signal has left the pending set,
// and the loop's check at the top of every phase is what must then stop the
// chain.
//
// The stderr pipe, and not the config file the coordinator reads, is the
// rendezvous because a config layer that is not a regular file is skipped
// unread: a FIFO config no longer holds the coordinator, and the run went on
// with the defaults before the signal was sent.
//
// The record must name NO phase — none started — and must say which phase was
// never reached. Without the check the next phase is started under the
// cancelled context, dies with "context canceled", and is recorded as a
// failure of work that never ran.
func TestLifecycleInterruptBetweenPhasesStartsNoPhase(t *testing.T) {
	s := newSignalSandbox(t)
	s.seed(t)
	if err := os.WriteFile(s.configPath(), []byte(signalTestConfig), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	r, w, prefill := parkingPipe(t)
	c := s.startCoordinatorTo(t, w)
	// The coordinator holds its own copy; the test keeps only the read end, so
	// the drain below ends when the coordinator does.
	_ = w.Close()

	deadline := time.Now().Add(60 * time.Second)
	for {
		blocked, ok := blockedOnStderr(c.cmd.Process.Pid)
		if !ok {
			if c.hasExited() {
				t.Fatal("the coordinator exited before it blocked on its stderr")
			}
			t.Skip("cannot read the coordinator's /proc/<pid>/task/<tid>/syscall")
		}
		if blocked {
			break
		}
		if c.hasExited() {
			t.Fatal("the coordinator exited before it blocked on its stderr")
		}
		if time.Now().After(deadline) {
			t.Fatal("the coordinator never blocked on its stderr")
		}
		time.Sleep(time.Millisecond)
	}

	if err := c.cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal the coordinator: %v", err)
	}
	// The runtime's handler has taken the signal once it is no longer pending.
	// What this cannot see is the step after it: the signal reaches
	// NotifyContext's goroutine, which cancels the root context, a couple of
	// goroutine hand-offs later, and the coordinator exposes nothing an outside
	// test can read when that lands — no log line, no file, no marker until the
	// run is over. So the pipe is drained only after signalCancelMargin on top.
	//
	// That margin is why this is safe on a slow -race runner. For its whole
	// length the coordinator's main goroutine is blocked in its stderr write,
	// with its thread handed back to the scheduler, so nothing in the process
	// competes with the goroutine doing the cancel, and a scheduling delay of a
	// quarter of a second for a runnable goroutine in an otherwise idle process
	// is not something a loaded machine produces. The coordinator's remaining
	// pre-loop work after the write (the audit sidecar sweep, the start stamp)
	// adds to it. The built binary is not race-instrumented, whatever flags the
	// test runs under. A run where the cancel still lost would fail loudly, as
	// resolve "interrupted after" instead of "interrupted before", rather than
	// pass.
	deadline = time.Now().Add(10 * time.Second)
	for {
		pending, ok := signalPending(c.cmd.Process.Pid, syscall.SIGINT)
		if !ok || !pending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the coordinator never took the SIGINT")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(signalCancelMargin)
	if c.hasExited() {
		t.Fatal("the SIGINT ended the coordinator while it was blocked on its stderr: it was not caught")
	}

	drained := make(chan error, 1)
	go func() {
		if _, err := io.CopyN(io.Discard, r, int64(prefill)); err != nil {
			drained <- fmt.Errorf("drain the prefill: %w", err)
			return
		}
		_, err := io.Copy(c.stderr, r)
		drained <- err
	}()

	c.waitExit(t, 60*time.Second)
	if c.code != 1 {
		t.Errorf("a run interrupted before its first phase exited %d, want 1", c.code)
	}
	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("read the coordinator's stderr: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the coordinator's stderr was still open 30s after it exited")
	}
	stderr := c.stderr.String()

	// No phase started: the harness was never called, and no phase reported.
	if _, err := os.Stat(filepath.Join(s.bin, "harness.argv")); err == nil {
		t.Errorf("a phase reached its harness after the interrupt\n%s", stderr)
	}
	for _, phase := range []string{"resolve", "supersede"} {
		for _, verb := range []string{" failed after ", " completed in ", " interrupted after "} {
			if strings.Contains(stderr, coordinatorLine(phase+verb)) {
				t.Errorf("phase %s ran after the interrupt (%q)\n%s", phase, verb, stderr)
			}
		}
	}
	if !strings.Contains(stderr, coordinatorLine("interrupted before resolve; not starting the remaining phases")) {
		t.Errorf("the coordinator did not stop before the first phase\n%s", stderr)
	}

	m := s.readMarker(t, c)
	if len(m.PhasesFailed) != 0 {
		t.Errorf("phases_failed = %v, want none: no phase started, so none may be blamed (error %q)", m.PhasesFailed, m.Error)
	}
	if want := mcpinit.InterruptedBySignal + " before resolve"; m.Error != want {
		t.Errorf("marker error = %q, want %q", m.Error, want)
	}
}
