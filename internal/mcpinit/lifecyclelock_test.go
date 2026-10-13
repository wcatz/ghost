package mcpinit

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
	// Nothing anywhere under the data dir may be named after the unsafe id: a
	// pid file, its .tmp sibling, or its .lock must never have been created.
	if err := filepath.WalkDir(dataHome, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.Contains(d.Name(), "evil") {
			t.Errorf("unsafe project id produced a path: %s", p)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestAcquireLifecycleLockReportsAnUnevaluatableClaimAsAnError pins the third
// outcome for a claim that could not be evaluated. A directory sitting where the
// claim's ".lock" file belongs cannot be opened, so nothing can say whether a run
// holds the slot — and that is not the same answer as "another live run holds
// it". Reported as the second, the coordinator printed "already running" and
// exited with no phase run, for as long as the directory stayed: maintenance
// stopped for the project while naming a cause that did not exist. The documented
// direction for an unknown is "run": the lock returns a no-op release, true and
// the error, so the caller warns and runs the chain.
func TestAcquireLifecycleLockReportsAnUnevaluatableClaimAsAnError(t *testing.T) {
	dataHome := isolatedHome(t)
	seedProject(t, dataHome, "proj-123", "/tmp/my-project", "My Project")
	lockPath := filepath.Join(dataHome, "ghost", "lifecycle-proj-123.pid.lock")
	if err := os.Mkdir(lockPath, 0o700); err != nil {
		t.Fatal(err)
	}

	release, ok, err := AcquireLifecycleLock("proj-123")
	if !ok || err == nil {
		t.Fatalf("an unopenable claim lock must fail open with an error, not read as a live holder (ok=%v err=%v)", ok, err)
	}
	if release == nil {
		t.Fatal("the fail-open outcome must hand back a callable release")
	}
	release()

	// The slot is free again once the obstruction is gone: the same call claims.
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	release, ok, err = AcquireLifecycleLock("proj-123")
	if !ok || err != nil {
		t.Fatalf("with the obstruction removed the lock must be claimed (ok=%v err=%v)", ok, err)
	}
	release()
}

// TestAcquireLifecycleLockRefusesAnOverlongProjectID pins the other trigger: a
// project id the store accepts but whose claim file name the filesystem does
// not. lifecycle-<id>.pid.lock past NAME_MAX cannot be created, and before the
// fix that read as "another lifecycle is already running" — every run, forever.
// The id is refused as a filename component before any path is built from it,
// which is the fail-open outcome with an error, and the refusal does not repeat
// the whole id back.
func TestAcquireLifecycleLockRefusesAnOverlongProjectID(t *testing.T) {
	dataHome := isolatedHome(t)
	id := strings.Repeat("p", 300)
	seedProject(t, dataHome, id, "/tmp/long-project", "Long Project")

	release, ok, err := AcquireLifecycleLock(id)
	if !ok || err == nil {
		t.Fatalf("an over-long project id must fail open with an error, not read as a live holder (ok=%v err=%v)", ok, err)
	}
	if release == nil {
		t.Fatal("the fail-open outcome must hand back a callable release")
	}
	release()
	if strings.Contains(err.Error(), id) {
		t.Errorf("the refusal repeats the whole %d-byte id: %v", len(id), err)
	}
}
