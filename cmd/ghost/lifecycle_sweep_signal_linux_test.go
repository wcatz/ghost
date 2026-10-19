//go:build linux

package main

// The signal half of the pre-loop tests. It is Linux only because it parks the
// coordinator the way lifecycle_signal_linux_test.go does, on a full stderr
// pipe, and reuses that file's pipe, /proc and signal helpers.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/mcpinit"
	"github.com/wcatz/ghost/internal/memory"
)

// preLoopProject is seven characters on purpose: the parked pipe leaves room
// for the first coordinator line only up to stderrParkHeadroom, and a longer
// name pushes that line past it, so the coordinator would block before its
// signal handler exists.
const preLoopProject = "preloop"

// seedPreLoopProject registers the project in the sandbox's store. Without it
// the per-project lock cannot resolve the name and says so on stderr BEFORE the
// signal handler is installed, so the parked pipe would hold the coordinator
// at a point where SIGTERM is still fatal.
func seedPreLoopProject(t *testing.T, s lifecycleSandbox) {
	t.Helper()
	dir := filepath.Join(s.dataHome, "ghost")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := memory.OpenDB(filepath.Join(dir, "ghost.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	store := memory.NewStore(db, nil)
	defer store.Close() //nolint:errcheck
	if err := store.EnsureProject(context.Background(), preLoopProject, filepath.Join(s.root, "work"), preLoopProject); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
}

// A SIGTERM that lands before the phase loop ends the run as an
// interruption before its first phase, exactly as one between phases does,
// and the steps still ahead of the loop do no further work: the retrieval
// audit, which writes to the store, does not run.
//
// The coordinator is held by its stderr, a pipe the test has filled to within
// stderrParkHeadroom of its size: the run's first line fits and its second
// does not, so the coordinator blocks in that write, after the signal handler
// is installed and before the phase loop. A config layer that is not a regular
// file is skipped unread, so a FIFO config.yaml no longer holds it. The pipe is
// drained only after the signal has left the pending set and
// signalCancelMargin has passed, because the handler cancels the root context
// asynchronously and nothing outside the process can observe that it has.
func TestLifecycleSignalBeforeThePhaseLoopIsAnInterruption(t *testing.T) {
	s := newLifecycleSandbox(t)
	if err := os.WriteFile(s.configFile, []byte(phasesConfig(true)), 0o600); err != nil {
		t.Fatal(err)
	}
	signals := filepath.Join(s.tmp, "ghost-audit-absent.signals")
	seedPreLoopProject(t, s)

	r, w, prefill := parkingPipe(t)
	ctx, cancel := context.WithTimeout(context.Background(), lifecycleRunDeadline)
	defer cancel()
	cmd := s.command(ctx, "--project", preLoopProject, "--source", "claude-code", "--signals", signals)
	cmd.Stderr = w
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// The coordinator holds its own copy; the test keeps only the read end, so
	// the drain below ends when the coordinator does.
	_ = w.Close()
	pid := cmd.Process.Pid
	defer coordinatorGone(t, pid)

	exited := make(chan struct{})
	go func() {
		defer close(exited)
		_ = cmd.Wait()
	}()
	hasExited := func() bool {
		select {
		case <-exited:
			return true
		default:
			return false
		}
	}

	for {
		blocked, ok := blockedOnStderr(pid)
		if !ok {
			if hasExited() {
				t.Fatal("the coordinator exited before it blocked on its stderr")
			}
			_ = cmd.Process.Kill()
			<-exited
			t.Skip("cannot read the coordinator's /proc/<pid>/task/<tid>/syscall")
		}
		if blocked {
			break
		}
		if hasExited() || ctx.Err() != nil {
			<-exited
			t.Fatal("the coordinator exited, or never blocked, on its stderr")
		}
		time.Sleep(time.Millisecond)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	for {
		pending, ok := signalPending(pid, syscall.SIGTERM)
		if !ok || !pending {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("the coordinator never took the SIGTERM")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(signalCancelMargin)
	if hasExited() {
		t.Fatal("the SIGTERM ended the coordinator while it was blocked on its stderr: it was not caught")
	}

	var out bytes.Buffer
	drained := make(chan error, 1)
	go func() {
		if _, err := io.CopyN(io.Discard, r, int64(prefill)); err != nil {
			drained <- fmt.Errorf("drain the prefill: %w", err)
			return
		}
		_, err := io.Copy(&out, r)
		drained <- err
	}()
	if err := <-drained; err != nil {
		t.Fatalf("read the coordinator's stderr: %v", err)
	}
	<-exited
	if ctx.Err() != nil {
		t.Fatalf("the coordinator did not finish within %s after SIGTERM:\n%s", lifecycleRunDeadline, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "interrupted before resolve") {
		t.Errorf("the run was not ended as interrupted before its first phase:\n%s", got)
	}
	if strings.Contains(got, "retrieval audit") {
		t.Errorf("the retrieval audit ran after the signal:\n%s", got)
	}

	markers, _ := filepath.Glob(filepath.Join(s.dataHome, "ghost", "lifecycle-last-failure*.json"))
	if len(markers) != 1 {
		t.Fatalf("want one failure marker, found %d: %v", len(markers), markers)
	}
	raw, err := os.ReadFile(markers[0])
	if err != nil {
		t.Fatal(err)
	}
	if want := mcpinit.InterruptedBySignal + " before resolve"; !strings.Contains(string(raw), want) {
		t.Errorf("the failure marker does not record %q:\n%s", want, raw)
	}
}
