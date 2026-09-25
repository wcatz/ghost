package fileguard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestQuarantineRestoresWhenGraceStampFails(t *testing.T) {
	oldChtimes := chtimesFile
	chtimesFile = func(string, time.Time, time.Time) error { return errors.New("stamp failed") }
	t.Cleanup(func() { chtimesFile = oldChtimes })
	restore := SetProbeForTest(func(string) (bool, error) { return false, nil })
	defer restore()
	root := t.TempDir()
	path := filepath.Join(root, "candidate")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Quarantine(path, currentProbe()); err == nil {
		t.Fatal("Quarantine ignored grace timestamp failure")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("candidate was not restored: %v", err)
	}
}

func TestQuarantineMarkerSurvivesReapAndSelfHeals(t *testing.T) {
	root := t.TempDir()
	dir, err := QuarantineDir(filepath.Join(root, "candidate"))
	if err != nil {
		t.Fatal(err)
	}
	owner := filepath.Join(dir, quarantineOwnerName)
	old := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(owner, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := ReapStaleQuarantineWithProbe(root, func(string) (bool, error) { return false, nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(owner); err != nil {
		t.Fatalf("reaper removed ownership marker: %v", err)
	}
	if err := os.Remove(owner); err != nil {
		t.Fatal(err)
	}
	if _, err := QuarantineDir(filepath.Join(root, "candidate")); err != nil {
		t.Fatalf("QuarantineDir did not self-heal marker: %v", err)
	}
	if !quarantineOwned(dir) {
		t.Fatal("ownership marker was not restored")
	}
}

func TestUnownedQuarantineDirIsNeverReaped(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".ghost-quarantine")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "user-file")
	if err := os.WriteFile(old, []byte("user"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(old, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if _, err := ReapStaleQuarantineWithProbe(root, func(string) (bool, error) { return false, nil }); err == nil {
		t.Fatal("reaper adopted an unowned quarantine directory")
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("unowned file removed: %v", err)
	}
	if _, err := QuarantineDir(filepath.Join(root, "candidate")); err == nil {
		t.Fatal("quarantine adopted an unowned directory")
	}
}

func TestTryAcquireLockContextIsBounded(t *testing.T) {
	root := t.TempDir()
	lockPath := filepath.Join(root, "busy.lock")
	held, err := AcquireLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close() //nolint:errcheck
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	lock, acquired, err := TryAcquireLockContext(ctx, lockPath)
	if lock != nil || acquired || err == nil {
		t.Fatalf("contended lock = lock=%v acquired=%v err=%v", lock, acquired, err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("contended acquisition took %v", elapsed)
	}
}

func TestPublishNoReplaceFallbackPreservesInterveningWriter(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skip("atomic no-replace fallback is implemented for Linux and Windows")
	}
	root := t.TempDir()
	stage := filepath.Join(root, "stage")
	path := filepath.Join(root, "visible")
	if err := os.WriteFile(stage, []byte("tail"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldLink := linkFile
	linkFile = func(_, _ string) error {
		if err := os.WriteFile(path, []byte("writer"), 0o600); err != nil {
			return err
		}
		return errors.New("hard links unavailable")
	}
	t.Cleanup(func() { linkFile = oldLink })

	if err := PublishNoReplace(stage, path); !errors.Is(err, ErrPublishExists) {
		t.Fatalf("PublishNoReplace err=%v, want ErrPublishExists", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "writer" {
		t.Fatalf("intervening writer file = %q err=%v, want preserved", data, err)
	}
}

func TestQuarantineGraceStartsAtQuarantineNotSourceMtime(t *testing.T) {
	restore := SetProbeForTest(func(string) (bool, error) { return false, nil })
	defer restore()
	root := t.TempDir()
	path := filepath.Join(root, "candidate")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	tombstone, err := Quarantine(path, currentProbe())
	if err != nil {
		t.Fatal(err)
	}
	removed, err := ReapStaleQuarantine(root)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("removed=%d, want recent tombstone preserved", removed)
	}
	if _, err := os.Stat(tombstone); err != nil {
		t.Fatalf("tombstone missing: %v", err)
	}
}

func TestReapStaleQuarantineOnlyOldUnheld(t *testing.T) {
	root := t.TempDir()
	dir, err := QuarantineDir(filepath.Join(root, "placeholder"))
	if err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "tombstone-old-1")
	recent := filepath.Join(dir, "tombstone-recent-1")
	for _, path := range []string{old, recent} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	oldTime := time.Now().Add(-2 * QuarantineGrace)
	if err := os.Chtimes(old, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	removed, err := ReapStaleQuarantineWithProbe(root, func(string) (bool, error) { return false, nil })
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed=%d want 1", removed)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old tombstone survived: %v", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Fatalf("recent tombstone removed: %v", err)
	}
}
