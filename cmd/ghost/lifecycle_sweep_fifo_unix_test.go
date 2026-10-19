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
	if !strings.Contains(stderr.String(), "audit sidecar sweep removed=0") {
		t.Errorf("the run did not report the sweep passing over the FIFO:\n%s", stderr.String())
	}
	if _, err := os.Lstat(fifo); err != nil {
		t.Errorf("the sweep removed a FIFO it never identified as a sidecar: %v", err)
	}
}
