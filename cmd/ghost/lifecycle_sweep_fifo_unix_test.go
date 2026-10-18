//go:build !windows

package main

// The coordinator's steps before its phase loop run outside every phase
// deadline, so a step that blocks there holds the per-project claim for
// good. A FIFO named like an audit sidecar in the temp dir did exactly that:
// the stale-sidecar sweep opened it and waited for a writer that never came.
// And because the coordinator catches SIGTERM, `kill <pid>` no longer ended
// it. These tests drive the built binary, because the hang and the signal
// are properties of the process, not of any one function.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/mcpinit"
)

// lifecycleRunDeadline bounds one coordinator run. A run here does no phase
// work, so this is only ever reached by a hang.
const lifecycleRunDeadline = 15 * time.Second

// lifecycleSandbox builds the binary and a sandbox around it: every
// directory the coordinator resolves is inside the test's temp dir, PATH is
// an empty directory so no CLI harness can be found, and the environment is
// built from scratch rather than filtered from this process's own.
type lifecycleSandbox struct {
	bin, root, tmp, configFile, dataHome string
}

func newLifecycleSandbox(t *testing.T) lifecycleSandbox {
	t.Helper()
	if testing.Short() {
		t.Skip("builds the ghost binary")
	}
	root := t.TempDir()
	bin := filepath.Join(root, "ghost")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	s := lifecycleSandbox{
		bin:        bin,
		root:       root,
		tmp:        filepath.Join(root, "tmp"),
		configFile: filepath.Join(root, "config", "ghost", "config.yaml"),
		dataHome:   filepath.Join(root, "data"),
	}
	for _, d := range []string{s.tmp, filepath.Dir(s.configFile), s.dataHome, filepath.Join(root, "empty-path")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func (s lifecycleSandbox) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, s.bin, append([]string{"lifecycle"}, args...)...)
	cmd.Env = []string{
		"HOME=" + s.root,
		"XDG_CONFIG_HOME=" + filepath.Join(s.root, "config"),
		"XDG_DATA_HOME=" + s.dataHome,
		"XDG_STATE_HOME=" + filepath.Join(s.root, "state"),
		"XDG_CACHE_HOME=" + filepath.Join(s.root, "cache"),
		"TMPDIR=" + s.tmp,
		"PATH=" + filepath.Join(s.root, "empty-path"),
		"GHOST_DEV_FORBID_DATA_DIR=" + os.Getenv("GHOST_DEV_FORBID_DATA_DIR"),
		"GHOST_OLLAMA_URL=http://127.0.0.1:1",
		"CLAUDE_CODE_SESSION_ID=",
	}
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

// phasesConfig names every automatic phase explicitly, so no layer beneath
// the user file can enable one this test did not ask for.
func phasesConfig(resolve bool) string {
	r := "false"
	if resolve {
		r = "true"
	}
	return "reflection:\n  auto_reflect: false\n  auto_resolve: " + r + "\n  auto_supersede: false\n"
}

// coordinatorGone fails the test when pid is still alive, so a hung coordinator is
// never left behind.
func coordinatorGone(t *testing.T, pid int) {
	t.Helper()
	for i := 0; i < 50; i++ {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("the coordinator (pid %d) is still alive", pid)
}

func TestLifecycleRunsPastAFIFOSidecarInTheTempDir(t *testing.T) {
	s := newLifecycleSandbox(t)
	if err := os.WriteFile(s.configFile, []byte(phasesConfig(false)), 0o600); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(s.tmp, "ghost-audit-planted.signals")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(fifo, old, old); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), lifecycleRunDeadline)
	defer cancel()
	cmd := s.command(ctx, "--project", "sweep-fifo")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait()
	coordinatorGone(t, pid)
	if ctx.Err() != nil {
		t.Fatalf("the coordinator did not finish within %s; it is blocked on the FIFO sidecar:\n%s", lifecycleRunDeadline, stderr.String())
	}
	if !strings.Contains(stderr.String(), "lifecycle: audit sidecar sweep removed=0") {
		t.Errorf("the run did not report the sweep passing over the FIFO:\n%s", stderr.String())
	}
	if _, err := os.Lstat(fifo); err != nil {
		t.Errorf("the sweep removed a FIFO it never identified as a sidecar: %v", err)
	}
}

// A SIGTERM that lands before the phase loop ends the run as an
// interruption before its first phase, exactly as one between phases does,
// and the steps still ahead of the loop do no further work: the retrieval
// audit, which writes to the store, does not run.
//
// The coordinator is held deterministically by making config.yaml a FIFO.
// Its config read opens that FIFO after the signal handler is installed, so
// when this test's open for writing returns, the coordinator is inside the
// read and SIGTERM is caught rather than fatal. The one wait that is not a
// handshake is the short pause after the signal: the handler cancels the
// root context asynchronously and nothing outside the process can observe
// that it has.
func TestLifecycleSignalBeforeThePhaseLoopIsAnInterruption(t *testing.T) {
	s := newLifecycleSandbox(t)
	if err := syscall.Mkfifo(s.configFile, 0o600); err != nil {
		t.Fatal(err)
	}
	signals := filepath.Join(s.tmp, "ghost-audit-absent.signals")

	ctx, cancel := context.WithTimeout(context.Background(), lifecycleRunDeadline)
	defer cancel()
	cmd := s.command(ctx, "--project", "pre-loop-signal", "--signals", signals)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	defer coordinatorGone(t, pid)

	opened := make(chan *os.File, 1)
	go func() {
		w, err := os.OpenFile(s.configFile, os.O_WRONLY, 0)
		if err != nil {
			opened <- nil
			return
		}
		opened <- w
	}()
	var w *os.File
	select {
	case w = <-opened:
	case <-ctx.Done():
		// Nothing ever opened the config to read it. Release the writer's
		// open with a reader of our own, so the goroutine does not outlive
		// the test, and let the deadline's kill end the coordinator.
		if r, err := os.OpenFile(s.configFile, os.O_RDONLY|syscall.O_NONBLOCK, 0); err == nil {
			if w := <-opened; w != nil {
				_ = w.Close()
			}
			_ = r.Close()
		}
		_ = cmd.Wait()
		t.Fatalf("the coordinator never read its config:\n%s", stderr.String())
	}
	if w == nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("could not open the config FIFO for writing")
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := w.WriteString(phasesConfig(true)); err != nil {
		t.Errorf("write the config: %v", err)
	}
	_ = w.Close()

	_ = cmd.Wait()
	if ctx.Err() != nil {
		t.Fatalf("the coordinator did not finish within %s after SIGTERM:\n%s", lifecycleRunDeadline, stderr.String())
	}
	out := stderr.String()
	if !strings.Contains(out, "lifecycle: interrupted before resolve") {
		t.Errorf("the run was not ended as interrupted before its first phase:\n%s", out)
	}
	if strings.Contains(out, "retrieval audit") {
		t.Errorf("the retrieval audit ran after the signal:\n%s", out)
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
