package mcpinit

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// Issue #730's refusal. `ghost history compact` rewrites memory_history and
// moves memories.updated_at, and the per-project lifecycle lock is the one thing
// that says no lifecycle run is currently appending to either. A reader of the
// lock is a different operation from AcquireLifecycleLock's claim, and it is
// exported for exactly that reason: a command that must NOT take the lock has to
// be able to ask about it, and asking must not claim.

// writeLockClaim writes projectID's pid file naming pid, in the same bare-pid
// form the lock used before creation-time tokens existed — so the reader is
// exercised against the shape a store written by an older Ghost still carries.
func writeLockClaim(t *testing.T, dataDir, projectID string, pid int) string {
	t.Helper()
	path := filepath.Join(dataDir, "lifecycle-"+projectID+".pid")
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)), 0o600); err != nil {
		t.Fatalf("write lifecycle claim: %v", err)
	}
	return path
}

// TestLifecycleLockHeld_A live claim is a refusal and a dead one is not. The
// distinguishing case is a pid whose PROCESS is gone: a crashed run leaves its
// pid file behind, and treating that as held would turn a repair into something
// no operator could ever run again.
func TestLifecycleLockHeld_A(t *testing.T) {
	dataDir := t.TempDir()

	// Unclaimed: nothing to refuse.
	if LifecycleLockHeld(dataDir, "proj") {
		t.Error("an unclaimed project reports a held lifecycle lock")
	}

	// Held by this process, which is definitionally alive.
	writeLockClaim(t, dataDir, "proj", os.Getpid())
	if !LifecycleLockHeld(dataDir, "proj") {
		t.Error("a claim naming this live process must read as held")
	}

	// Held by another live process. `sleep 60` is used rather than pid 1 because
	// pid 1 answers EPERM to signal 0 as a non-root user and reads as dead.
	holder := exec.Command("sleep", "60")
	if err := holder.Start(); err != nil {
		t.Skipf("cannot start a holder process: %v", err)
	}
	t.Cleanup(func() { _ = holder.Process.Kill(); _, _ = holder.Process.Wait() })
	writeLockClaim(t, dataDir, "proj", holder.Process.Pid)
	if !LifecycleLockHeld(dataDir, "proj") {
		t.Error("a claim naming another live process must read as held")
	}

	// A claim whose process is gone, and a malformed one, are both "not held".
	// A reader that could not parse the claim and reported "held" would refuse
	// every run on a store with one corrupt file, with no way to clear it.
	dead := exec.Command("sleep", "60")
	if err := dead.Start(); err != nil {
		t.Skipf("cannot start a process to kill: %v", err)
	}
	pid := dead.Process.Pid
	_ = dead.Process.Kill()
	_, _ = dead.Process.Wait()
	writeLockClaim(t, dataDir, "proj", pid)
	if LifecycleLockHeld(dataDir, "proj") {
		t.Error("a claim naming a dead process must not read as held, or a crashed run blocks the repair forever")
	}
	writeLockClaim(t, dataDir, "proj", -1)
	if LifecycleLockHeld(dataDir, "proj") {
		t.Error("a malformed claim must not read as held")
	}
}

// TestLifecycleLockHeld_PerProject: the claim is per project, and the reader is
// asked per project. A reader that ignored its argument would refuse a repair of
// an idle project because a busy one is running, and --project would be a flag
// that changes nothing.
func TestLifecycleLockHeld_PerProject(t *testing.T) {
	dataDir := t.TempDir()
	writeLockClaim(t, dataDir, "busy", os.Getpid())

	if !LifecycleLockHeld(dataDir, "busy") {
		t.Error("the claiming project must read as held")
	}
	if LifecycleLockHeld(dataDir, "idle") {
		t.Error("another project must not read as held: the claim is per project")
	}
}

// TestLifecycleLockHeld_ReadsNothing: a check exists to decide, and a check that
// writes a file of its own would be a claim wearing a question's clothes — a
// dry run that creates a lock file is a dry run that changed the store.
func TestLifecycleLockHeld_ReadsNothing(t *testing.T) {
	dataDir := t.TempDir()
	before, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatalf("read data dir: %v", err)
	}
	for i := 0; i < 3; i++ {
		LifecycleLockHeld(dataDir, "proj")
	}
	after, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatalf("read data dir: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("reading the lock created %d file(s); it must create none", len(after)-len(before))
	}
}

// TestLifecycleLockHeld_UnsafeIDNeverJoinsAPath: the claim path is built from an
// unconstrained project id, so a reader that joins an id into a path is a reader
// that can be pointed outside the data dir. AcquireLifecycleLock validates the
// id; the reader has to as well, and to report "not held" rather than to stat
// whatever the join produced.
func TestLifecycleLockHeld_UnsafeIDNeverJoinsAPath(t *testing.T) {
	inner := t.TempDir()
	dataDir := filepath.Join(inner, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	outside := filepath.Join(inner, "outside.pid")
	if err := os.WriteFile(outside, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatalf("write the file outside the data dir: %v", err)
	}

	if LifecycleLockHeld(dataDir, "../../outside") {
		t.Error("an unsafe project id reported a held lock by escaping the data dir")
	}
	if LifecycleLockHeld(dataDir, "") {
		t.Error("an empty project id must not report a held lock")
	}
}
