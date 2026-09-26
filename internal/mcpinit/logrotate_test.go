package mcpinit

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedOversizedLog writes exactly cap bytes to path (a marker first line, then
// padding) and returns the full content, so a test can prove the file that was
// there is the one that moved.
func seedOversizedLog(t *testing.T, path, marker string) string {
	t.Helper()
	body := marker + "\n"
	pad := bytes.Repeat([]byte("x"), logRotateCap-len(body))
	content := body + string(pad)
	if len(content) != logRotateCap {
		t.Fatalf("seeded %d bytes, want exactly the cap %d", len(content), logRotateCap)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("seed oversized log: %v", err)
	}
	return content
}

// TestOpenLogForAppendRotatesAtCap: an append-only log with no writer to bound
// it grows forever. Opening one that has reached the cap starts a fresh file
// and leaves the full one beside it as "<name>.1", replacing whatever ".1"
// held — one rotation, not a chain of them.
func TestOpenLogForAppendRotatesAtCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lifecycle.log")
	content := seedOversizedLog(t, path, "the big one")
	stale := "a previous rotation's copy"
	if err := os.WriteFile(path+".1", []byte(stale), 0o600); err != nil {
		t.Fatalf("seed stale .1: %v", err)
	}

	f, err := openLogForAppend(path)
	if err != nil {
		t.Fatalf("openLogForAppend: %v", err)
	}
	if _, err := f.Write([]byte("fresh line\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if string(got) != "fresh line\n" {
		t.Errorf("after rotation the log holds %q, want only the line written to the fresh file", got)
	}
	rotated, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("read rotated copy: %v", err)
	}
	if string(rotated) != content {
		t.Errorf(".1 must hold the rotated content (%d bytes), got %d bytes (stale copy not replaced?)", len(content), len(rotated))
	}
}

// TestOpenLogForAppendLeavesSmallLogAlone: rotation only happens at the cap.
// A log still under it keeps its history — appending is the whole point of
// opening it.
func TestOpenLogForAppendLeavesSmallLogAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lifecycle.log")
	if err := os.WriteFile(path, []byte("early\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	stale := "an older rotation"
	if err := os.WriteFile(path+".1", []byte(stale), 0o600); err != nil {
		t.Fatalf("seed .1: %v", err)
	}

	f, err := openLogForAppend(path)
	if err != nil {
		t.Fatalf("openLogForAppend: %v", err)
	}
	if _, err := f.Write([]byte("later\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if string(got) != "early\nlater\n" {
		t.Errorf("log holds %q, want the original line plus the append", got)
	}
	rotated, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("read .1: %v", err)
	}
	if string(rotated) != stale {
		t.Errorf(".1 was rewritten (%q) on a log that had not reached the cap", rotated)
	}
}

// TestOpenLogForAppendCreatesMissingLog: the common case for a fresh install —
// no log yet, so there is nothing to rotate and the file is simply created.
func TestOpenLogForAppendCreatesMissingLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "obsidian-sync.log")

	f, err := openLogForAppend(path)
	if err != nil {
		t.Fatalf("openLogForAppend: %v", err)
	}
	if _, err := f.Write([]byte("first\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "first\n" {
		t.Errorf("log holds %q (err %v), want %q", b, err, "first\n")
	}
	if _, err := os.Lstat(path + ".1"); !os.IsNotExist(err) {
		t.Errorf("a missing log must not produce a .1, Lstat err = %v", err)
	}
}

// TestOpenLogForAppendSkipsSymlink: rotation is decided by Lstat, so a symlink
// wearing the log's name is never renamed — renaming it would move the link
// out from under whatever arranged it, and the file it points at keeps
// growing, which is the caller's problem, not this function's.
func TestOpenLogForAppendSkipsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.log")
	seedOversizedLog(t, target, "through a link")
	path := filepath.Join(dir, "lifecycle.log")
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("seed symlink: %v", err)
	}

	f, err := openLogForAppend(path)
	if err != nil {
		t.Fatalf("openLogForAppend: %v", err)
	}
	if _, err := f.Write([]byte("appended\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat log: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the symlink was replaced by a regular file (mode %v)", fi.Mode())
	}
	if _, err := os.Lstat(path + ".1"); !os.IsNotExist(err) {
		t.Errorf("rotating a symlink would rename the link itself; .1 exists, Lstat err = %v", err)
	}
	if b, err := os.ReadFile(target); err != nil || !strings.HasSuffix(string(b), "appended\n") {
		t.Errorf("the target must still receive the append, got %q (err %v)", b, err)
	}
}

// TestOpenLogForAppendRotationIsBestEffort: the rename can fail — a directory
// sitting at "<name>.1", a read-only mount, a file another process holds. The
// log is diagnostic, so the append still has to happen rather than the caller
// losing its log line (or, at the spawn sites, its spawned process's stdout).
func TestOpenLogForAppendRotationIsBestEffort(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lifecycle.log")
	seedOversizedLog(t, path, "cannot be moved")
	if err := os.Mkdir(path+".1", 0o700); err != nil {
		t.Fatalf("seed blocking directory: %v", err)
	}

	f, err := openLogForAppend(path)
	if err != nil {
		t.Fatalf("openLogForAppend must not fail because rotation could not: %v", err)
	}
	if _, err := f.Write([]byte("still logged\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.HasSuffix(string(b), "still logged\n") {
		t.Errorf("append was lost when rotation failed; log ends %q", b)
	}
	if fi, err := os.Stat(path + ".1"); err != nil || !fi.IsDir() {
		t.Errorf("the blocking directory must be left as it was, stat err = %v", err)
	}
}

// TestLogLifecycleCooldownSkipRotatesOversizedLog: the wiring, not just the
// helper. The skip line goes through the same open as every other lifecycle
// line, so a lifecycle.log that has grown past the cap is rotated before that
// line lands — otherwise the cap would hold for everything except the one path
// that writes most often.
func TestLogLifecycleCooldownSkipRotatesOversizedLog(t *testing.T) {
	dataDir := t.TempDir()
	path := filepath.Join(dataDir, "lifecycle.log")
	content := seedOversizedLog(t, path, "543 runs in one lifecycle.log")

	logLifecycleCooldownSkip(dataDir, "p1", 12*time.Minute, 30*time.Minute)

	rotated, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("read rotated copy: %v", err)
	}
	if string(rotated) != content {
		t.Errorf(".1 must hold the rotated log (%d bytes), got %d bytes", len(content), len(rotated))
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read lifecycle.log: %v", err)
	}
	line := string(b)
	for _, want := range []string{"p1", "12m0s", "min_interval"} {
		if !strings.Contains(line, want) {
			t.Errorf("the fresh log must carry the skip line %q, got %q", want, line)
		}
	}
}
