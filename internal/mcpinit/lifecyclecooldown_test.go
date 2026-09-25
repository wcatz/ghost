package mcpinit

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// lifecycle.min_interval (#541) stops the Stop hook spawning the
// reflect→resolve→supersede chain after every turn. The cooldown is recorded as
// a per-project stamp file in the data dir rather than a row in the database,
// because the hook's synchronous path must not open the store to ask a question
// it can answer with one stat.
//
// None of these tests spawn anything: the decision is a pure function of the
// stamp's mtime, and the stamp is written by the same helper the spawned
// `ghost lifecycle` calls. Calling runLifecycle() here would re-exec os.Args[0],
// which under `go test` is the test binary — a fork bomb (2026-09-25).

// writeStamp creates the stamp file for projectID with mtime exactly
// `age` before `now`, so the cooldown boundary is exercised without sleeping.
func writeStamp(t *testing.T, dataDir, projectID string, now time.Time, age time.Duration) string {
	t.Helper()
	path := filepath.Join(dataDir, lifecycleLastStartFile(projectID))
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("write stamp: %v", err)
	}
	stamp := now.Add(-age)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatalf("chtimes stamp: %v", err)
	}
	return path
}

// TestLifecycleCooldown_SkipsInsideTheWindow is the fix itself: a run that
// started a minute ago must keep the next turn from spawning another, so a
// chatty session cannot spend a harness call per turn (issue #541 counted 543
// lifecycle runs in one log).
func TestLifecycleCooldown_SkipsInsideTheWindow(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Now()
	writeStamp(t, dataDir, "p1", now, 90*time.Second)

	skip, since := lifecycleCooldownActive(dataDir, "p1", 30*time.Minute, now)
	if !skip {
		t.Error("a run 90s old must be skipped with a 30m cooldown")
	}
	if since < 89*time.Second || since > 91*time.Second {
		t.Errorf("since = %s, want the stamp's age (~90s)", since)
	}
}

// TestLifecycleCooldown_RunsAfterTheWindow: the cooldown bounds the rate, it
// does not disable consolidation.
func TestLifecycleCooldown_RunsAfterTheWindow(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Now()
	writeStamp(t, dataDir, "p1", now, 31*time.Minute)

	skip, _ := lifecycleCooldownActive(dataDir, "p1", 30*time.Minute, now)
	if skip {
		t.Error("a run 31m old must proceed with a 30m cooldown")
	}
}

// TestLifecycleCooldown_RunsWhenTheStampIsMissing: first ever turn, and the
// case where the stamp could not be written. A missing or unreadable file must
// mean "run" — a cooldown that could silently stop maintenance would be the
// worse failure.
func TestLifecycleCooldown_RunsWhenTheStampIsMissing(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Now()

	skip, _ := lifecycleCooldownActive(dataDir, "p1", 30*time.Minute, now)
	if skip {
		t.Error("a project with no stamp must run")
	}

	// A stamp path that cannot be read at all (here: a directory where the file
	// belongs) must degrade the same way rather than erroring or skipping.
	if err := os.Mkdir(filepath.Join(dataDir, lifecycleLastStartFile("p1")), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if skip, _ := lifecycleCooldownActive(dataDir, "p1", 30*time.Minute, now); skip {
		t.Error("an unreadable stamp must mean run, not skip")
	}
}

// TestLifecycleCooldown_ZeroAlwaysRuns pins the documented opt-out: 0 disables
// the cooldown entirely, so a project that wants every turn to consolidate
// still gets it. The future-dated stamp is the discriminating case — it is what
// separates "the opt-out short-circuits before the stamp is read" from "the
// window comparison happens to be false", since a negative age is less than
// every non-positive interval. A clock change must not re-enable a cooldown the
// user turned off.
func TestLifecycleCooldown_ZeroAlwaysRuns(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Now()
	writeStamp(t, dataDir, "p1", now, 0)
	writeStamp(t, dataDir, "p2", now, -time.Hour) // an hour in the future
	writeStamp(t, dataDir, "p3", now, 24*time.Hour)

	for _, min := range []time.Duration{0, -time.Minute} {
		for _, project := range []string{"p1", "p2", "p3"} {
			if skip, _ := lifecycleCooldownActive(dataDir, project, min, now); skip {
				t.Errorf("min_interval %s must always run (project %s)", min, project)
			}
		}
	}
}

// TestLifecycleCooldown_EmptyIdentitiesRun: with no data dir or no project
// there is nothing to key a cooldown on, so the caller must proceed to whatever
// it would have done before this guard existed.
func TestLifecycleCooldown_EmptyIdentitiesRun(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Now()
	writeStamp(t, dataDir, "p1", now, time.Second)

	if skip, _ := lifecycleCooldownActive(dataDir, "p1", time.Minute, now); !skip {
		t.Fatal("precondition: the stamp is inside the window")
	}
	if skip, _ := lifecycleCooldownActive("", "p1", time.Minute, now); skip {
		t.Error("an unknown data dir must run, not skip")
	}
	if skip, _ := lifecycleCooldownActive(dataDir, "", time.Minute, now); skip {
		t.Error("an unknown project must run, not skip")
	}
}

// TestLifecycleCooldown_FutureStampSkips: a stamp in the future (clock change,
// a file copied in) is not evidence that the cooldown expired, so it is treated
// as inside the window. The failure mode of the other choice — spawning on every
// turn again — is the one this whole change exists to remove.
func TestLifecycleCooldown_FutureStampSkips(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Now()
	writeStamp(t, dataDir, "p1", now, -time.Hour)

	skip, since := lifecycleCooldownActive(dataDir, "p1", 30*time.Minute, now)
	if !skip {
		t.Error("a stamp dated in the future must be inside the window")
	}
	if since >= 0 {
		t.Errorf("since = %s, want the negative age of a future stamp", since)
	}
}

// TestLifecycleCooldown_IsPerProject: one project's busy lifecycle must not
// delay another's.
func TestLifecycleCooldown_IsPerProject(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Now()
	writeStamp(t, dataDir, "p1", now, time.Minute)

	if skip, _ := lifecycleCooldownActive(dataDir, "p1", 30*time.Minute, now); !skip {
		t.Error("p1's own stamp must hold p1 back")
	}
	if skip, _ := lifecycleCooldownActive(dataDir, "p2", 30*time.Minute, now); skip {
		t.Error("p2 has no stamp of its own and must run")
	}
}

// TestLifecycleLastStartFile pins the file name: it sits beside the per-project
// pid lock, keyed on the RESOLVED project id (what both the hook and the
// coordinator use), and the stem is fixed so an operator can find it.
func TestLifecycleLastStartFile(t *testing.T) {
	if got, want := lifecycleLastStartFile("p1"), "lifecycle-p1.last"; got != want {
		t.Errorf("lifecycleLastStartFile(p1) = %q, want %q", got, want)
	}
	// Beside the pid lock, not replacing it: the two guards are independent and
	// both files must coexist for the same project.
	if lifecycleLastStartFile("p1") == "lifecycle-p1.pid" {
		t.Error("the stamp must not reuse the pid file")
	}
}

// TestTouchLifecycleStart pins the writer the spawned coordinator calls at the
// start of a run: it resolves the project the way the pid lock does (so a manual
// run by NAME and a hook-spawned run by ID land on the same stamp), writes it
// atomically with 0600, and leaves no temp file behind.
func TestTouchLifecycleStart(t *testing.T) {
	dataHome := isolatedHome(t)
	dataDir := filepath.Join(dataHome, "ghost")
	seedProject(t, dataHome, "proj-123", "/tmp/my-project", "My Project")

	if err := TouchLifecycleStart("My Project"); err != nil {
		t.Fatalf("TouchLifecycleStart: %v", err)
	}
	// Reached by name; the file is keyed on the resolved id, the same value the
	// hook's pidPath uses.
	path := filepath.Join(dataDir, lifecycleLastStartFile("proj-123"))
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("expected the stamp for the resolved id: %v", err)
	}
	if hasFileIdentity() {
		if perm := st.Mode().Perm(); perm != 0o600 {
			t.Errorf("stamp mode = %o, want 0600", perm)
		}
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("touch left a temp file behind: %s", e.Name())
		}
	}

	// The stamp must read back as a run that JUST happened, so the next turn is
	// inside the cooldown.
	if skip, since := lifecycleCooldownActive(dataDir, "proj-123", 30*time.Minute, time.Now()); !skip {
		t.Error("a freshly touched stamp must hold the next spawn back")
	} else if since > time.Minute || since < 0 {
		t.Errorf("since = %s, want a stamp that is seconds old", since)
	}

	// The cooldown moves with the stamp: touching again after the window has
	// passed must reset it.
	if err := TouchLifecycleStart("proj-123"); err != nil {
		t.Fatalf("second TouchLifecycleStart: %v", err)
	}
	if skip, _ := lifecycleCooldownActive(dataDir, "proj-123", 30*time.Minute, time.Now()); !skip {
		t.Error("re-touching must put the project back inside the window")
	}
}

// hasFileIdentity reports whether the platform can tell one file from another
// and one file's permissions from another's.
//
// It cannot on Windows, for two independently established reasons.
//
// The mode one is not in doubt: NTFS has no POSIX mode bits, so os.CreateTemp
// yields 0666 whatever perm the open asked for.
//
// The identity one is empirical: on windows-latest, os.SameFile(before, after)
// across this exact temp+rename compared EQUAL (CI run 36187098324, job
// 108243082501). I could not settle from here whether the NTFS file index is
// reused or the path is re-resolved, and it no longer matters: the point is that
// the check does not discriminate there, so it is not used as one.
//
// The portable alternative — hold a read handle open across the touch and check
// it still reads the old bytes — does not work either, and this is worth
// recording because it looks obviously right: syscall.Open on Windows uses
// `sharemode := FILE_SHARE_READ | FILE_SHARE_WRITE` (src/syscall/
// syscall_windows.go, syscall.Open), with NO FILE_SHARE_DELETE. An open handle
// therefore denies a rename over that path, and the test fails against correct
// code with "rename ... lifecycle-p1.last: Access is denied" (CI run 36188063873,
// job 108246513870). Any observation that needs the destination open cannot
// coexist with the rename it is trying to observe.
//
// So: the two hygiene properties — the stale content is gone and the mtime moved
// — are asserted everywhere, and the two DISCRIMINATING ones (a different file,
// mode 0600) run wherever the platform expresses them. Every mutation for this
// change was run on Linux, where they are live.
func hasFileIdentity() bool {
	return runtime.GOOS != "windows"
}

// TestTouchLifecycleStart_ReplacesTheStamp pins the properties a write-in-place
// would lose. The stale content must be gone and the mtime must MOVE, or a
// long-lived project would be pinned inside the window by a stamp nobody
// refreshed; and where the platform can express it (hasFileIdentity), the stamp
// must be a DIFFERENT file carrying 0600 — an in-place write keeps the old
// inode's identity and the old path's wider mode, so both checks fail for it.
func TestTouchLifecycleStart_ReplacesTheStamp(t *testing.T) {
	dataHome := isolatedHome(t)
	seedProject(t, dataHome, "p1", "/tmp/p1", "p1")
	dataDir := filepath.Join(dataHome, "ghost")
	path := filepath.Join(dataDir, lifecycleLastStartFile("p1"))

	// A stamp from a much older run, world-readable, with content in it.
	old := time.Now().Add(-6 * time.Hour)
	if err := os.WriteFile(path, []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	// Precondition: the stale stamp is outside any cooldown.
	if skip, _ := lifecycleCooldownActive(dataDir, "p1", 30*time.Minute, time.Now()); skip {
		t.Fatal("precondition: a 6h-old stamp is outside a 30m window")
	}

	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := TouchLifecycleStart("p1"); err != nil {
		t.Fatalf("TouchLifecycleStart: %v", err)
	}

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if hasFileIdentity() {
		if os.SameFile(before, st) {
			t.Error("the stamp is still the same file: it was written in place, not replaced by a rename")
		}
		if perm := st.Mode().Perm(); perm != 0o600 {
			t.Errorf("stamp mode = %o, want 0600 (an in-place write would keep the old mode)", perm)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "stale") {
		t.Errorf("stamp content = %q, want the previous content replaced", b)
	}
	if skip, since := lifecycleCooldownActive(dataDir, "p1", 30*time.Minute, time.Now()); !skip {
		t.Error("the refreshed stamp must hold the next spawn back")
	} else if since > time.Minute {
		t.Errorf("since = %s, want the refreshed (not 6h-old) stamp", since)
	}
}

// TestTouchLifecycleStart_UnknownProjectTouchesNothing: nothing can be resolved
// and no store is needed, so this must report an error rather than create a
// stamp named after an arbitrary string.
func TestTouchLifecycleStart_UnknownProjectTouchesNothing(t *testing.T) {
	dataHome := isolatedHome(t)
	seedProject(t, dataHome, "p1", "/tmp/p1", "p1")

	if err := TouchLifecycleStart("no-such-project"); err == nil {
		t.Error("an unresolvable project must report an error")
	}
	entries, err := os.ReadDir(filepath.Join(dataHome, "ghost"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".last") {
			t.Errorf("an unresolvable project produced a stamp: %s", e.Name())
		}
	}
}

// TestLogLifecycleCooldownSkip pins the one line the skip leaves behind. It
// goes to the existing lifecycle log, not stderr: the stop hook's stderr is the
// host's, and a line per turn there is noise the user cannot act on. The log is
// also the only record that the cooldown is why a run did not happen.
func TestLogLifecycleCooldownSkip(t *testing.T) {
	dataDir := t.TempDir()
	logLifecycleCooldownSkip(dataDir, "p1", 12*time.Minute, 30*time.Minute)

	b, err := os.ReadFile(filepath.Join(dataDir, "lifecycle.log"))
	if err != nil {
		t.Fatalf("read lifecycle.log: %v", err)
	}
	line := string(b)
	for _, want := range []string{"p1", "12m0s", "30m0s", "min_interval"} {
		if !strings.Contains(line, want) {
			t.Errorf("lifecycle.log line %q must mention %q", line, want)
		}
	}
	if n := strings.Count(strings.TrimRight(line, "\n"), "\n") + 1; n != 1 {
		t.Errorf("lifecycle.log holds %d lines, want exactly 1:\n%s", n, line)
	}

	// A second skip appends rather than truncating, so a log read later shows
	// the whole picture.
	logLifecycleCooldownSkip(dataDir, "p1", time.Minute, 30*time.Minute)
	b, err = os.ReadFile(filepath.Join(dataDir, "lifecycle.log"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(strings.TrimRight(string(b), "\n"), "\n") + 1; n != 2 {
		t.Errorf("lifecycle.log holds %d lines after two skips, want 2:\n%s", n, b)
	}
}

// TestLogLifecycleCooldownSkip_UnopenableLogIsNotFatal: the log is diagnostic.
// Failing to write it must not stop the skip (or anything else).
func TestLogLifecycleCooldownSkip_UnopenableLogIsNotFatal(t *testing.T) {
	// A data dir that does not exist: the append cannot be created.
	logLifecycleCooldownSkip(filepath.Join(t.TempDir(), "absent"), "p1", time.Minute, 30*time.Minute)
}
