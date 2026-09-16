package mcpinit

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// seedProject creates the data dir's database with one project, so that
// AcquireLifecycleLock can resolve an identifier the way a real run does.
func seedProject(t *testing.T, dataHome, id, path, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dataHome, "ghost"), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := memory.OpenDB(filepath.Join(dataHome, "ghost", "ghost.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck
	if err := memory.NewStore(db, nil).EnsureProject(context.Background(), id, path, name); err != nil {
		t.Fatal(err)
	}
}

// TestAcquireLifecycleLock pins the single-claimer contract. The coordinator
// claims the per-project lock itself — the hook deliberately does not pre-claim
// on its behalf, because it cannot write the child's pid before Start and a
// claim written in between would make the child read a foreign pid and abort,
// silently dropping the whole maintenance cycle. The claim is keyed on the
// RESOLVED project id, so a manual run by name and a hook-spawned run by id
// contend for the same file.
func TestAcquireLifecycleLock(t *testing.T) {
	dataHome := isolatedHome(t)
	seedProject(t, dataHome, "proj-123", "/tmp/my-project", "My Project")
	pidPath := filepath.Join(dataHome, "ghost", "lifecycle-proj-123.pid")

	// (a) unclaimed: a run by NAME resolves to the id and takes the id's file,
	// then release cleans up its own claim.
	release, ok, err := AcquireLifecycleLock("My Project")
	if err != nil {
		t.Fatalf("unexpected error acquiring an unclaimed lock: %v", err)
	}
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

	// (b) the same project reached by ID is the same lock.
	releaseByID, ok, err := AcquireLifecycleLock("proj-123")
	if err != nil || !ok {
		t.Errorf("expected to acquire the same project by id (ok=%v err=%v)", ok, err)
	}
	if !ok {
		t.Fatal("cannot continue without the claim")
	}
	releaseByID()
	// (c) another LIVE process holds it: refuse. Use a process we own — pid 1
	// answers EPERM to signal 0 as a non-root user and would read as dead.
	holder := exec.Command("sleep", "60")
	if err := holder.Start(); err != nil {
		t.Skipf("cannot start a holder process: %v", err)
	}
	t.Cleanup(func() { _ = holder.Process.Kill(); _, _ = holder.Process.Wait() })
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(holder.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := AcquireLifecycleLock("proj-123"); ok || err != nil {
		t.Errorf("must not acquire a lock held by a live process (ok=%v err=%v)", ok, err)
	}

	// (d) an unknown project has nothing to lock: proceed unlocked, no error —
	// the phases report the unknown project themselves.
	if err := os.Remove(pidPath); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := AcquireLifecycleLock("no-such-project"); !ok || err != nil {
		t.Errorf("unknown project must fail open (ok=%v err=%v)", ok, err)
	}

	// (e) an id that is not a safe filename component is never joined into a
	// path: refuse with an error (fail open, unlocked) and create nothing.
	seedProject(t, dataHome, "../../evil", "/tmp/evil", "Evil")
	if _, ok, err := AcquireLifecycleLock("../../evil"); err == nil || !ok {
		t.Errorf("unsafe project id must fail open with an error (ok=%v err=%v)", ok, err)
	}
	if entries, err := filepath.Glob(filepath.Join(dataHome, "*", "*evil*")); err != nil || len(entries) > 2 {
		t.Errorf("unsafe id produced path entries outside the data dir: %v (%v)", entries, err)
	}
}
