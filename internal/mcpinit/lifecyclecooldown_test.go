package mcpinit

import (
	"os"
	"path/filepath"
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

// TestLifecycleCooldown_FutureStampSkips: a stamp slightly in the future (small
// clock skew) is not evidence that the cooldown expired, so it is treated as
// inside the window rather than spawning on every turn again.
func TestLifecycleCooldown_FutureStampSkips(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Now()
	writeStamp(t, dataDir, "p1", now, -10*time.Minute)

	skip, since := lifecycleCooldownActive(dataDir, "p1", 30*time.Minute, now)
	if !skip {
		t.Error("a stamp slightly in the future must be inside the window")
	}
	if since >= 0 {
		t.Errorf("since = %s, want the negative age of a future stamp", since)
	}
}

// TestLifecycleCooldown_FarFutureStampRuns: a stamp more than one window ahead
// (a clock that was far ahead when it was written, a VM restore) would
// otherwise hold consolidation off until wall time caught up — days, or a year.
// It reads as stale instead.
func TestLifecycleCooldown_FarFutureStampRuns(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Now()
	writeStamp(t, dataDir, "p1", now, -time.Hour)

	if skip, _ := lifecycleCooldownActive(dataDir, "p1", 30*time.Minute, now); skip {
		t.Error("a stamp more than one window in the future must not block the run")
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
