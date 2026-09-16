package mcpinit

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// TestAcquireLifecycleLock pins the coordination between the two ways a
// lifecycle starts. The stop hook claims the pid file with the CHILD's pid
// before spawning, so the child finds its own pid and must proceed; a manual
// `ghost lifecycle` takes the claim itself, so it can no longer overlap a
// hook-spawned run for the same project (which happened while verifying).
func TestAcquireLifecycleLock(t *testing.T) {
	dataHome := isolatedHome(t)
	pidPath := filepath.Join(dataHome, "ghost", "lifecycle-proj.pid")

	// (a) unclaimed: this process takes it, and release cleans up its own claim.
	release, ok := AcquireLifecycleLock("proj")
	if !ok {
		t.Fatal("expected to acquire an unclaimed lifecycle lock")
	}
	if got := pidInFile(pidPath); got != os.Getpid() {
		t.Fatalf("pid file holds %d, want our pid %d", got, os.Getpid())
	}
	release()
	if got := pidInFile(pidPath); got != 0 {
		t.Errorf("release left its own claim behind (pid %d)", got)
	}

	// (b) another LIVE process holds it: refuse. A bare pid is the legacy
	// liveness-only format. Use a process we own — pid 1 answers EPERM to
	// signal 0 as a non-root user and would read as dead.
	holder := exec.Command("sleep", "60")
	if err := holder.Start(); err != nil {
		t.Skipf("cannot start a holder process: %v", err)
	}
	t.Cleanup(func() { _ = holder.Process.Kill(); _, _ = holder.Process.Wait() })
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(holder.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := AcquireLifecycleLock("proj"); ok {
		t.Error("must not acquire a lock held by a live process")
	}

	// (c) the hook claimed it for THIS pid: honour it, and do not delete it.
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	release2, ok := AcquireLifecycleLock("proj")
	if !ok {
		t.Fatal("a claim written for this process's own pid must be honoured")
	}
	release2()
	if got := pidInFile(pidPath); got != os.Getpid() {
		t.Errorf("a hook-written claim must survive the coordinator exiting (pid %d)", got)
	}
}
