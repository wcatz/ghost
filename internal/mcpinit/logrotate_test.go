package mcpinit

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// TestOpenLogForAppendKeepsWorkingWhileTheFilesystemStaysFull: the fallback is
// not a one-shot. While the disk stays short of room the log has no file under
// its own name, so EVERY later open hits the same create failure — and each one
// must still land in the rotated copy, rather than turning a degraded log into
// an error the spawn sites act on (one of them records a spawn failure and does
// not spawn at all). This is also the case the docs describe: the name returns
// only when the filesystem has room to create it again.
func TestOpenLogForAppendKeepsWorkingWhileTheFilesystemStaysFull(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lifecycle.log")
	content := seedOversizedLog(t, path, "written before the disk filled")

	origCreate := createLog
	createLog = func(p string) (*os.File, error) {
		if p == path {
			return nil, errors.New("no space left on device")
		}
		return origCreate(p)
	}
	t.Cleanup(func() { createLog = origCreate })

	logs := captureLogOutput(t)

	for _, line := range []string{"first line while full\n", "second line while full\n"} {
		f, err := openLogForAppend(path)
		if err != nil {
			t.Fatalf("an open while the filesystem is still full must not fail: %v", err)
		}
		if _, err := f.Write([]byte(line)); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}

	rotated, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("read rotated copy: %v", err)
	}
	if want := content + "first line while full\nsecond line while full\n"; string(rotated) != want {
		t.Errorf("rotated copy holds %d bytes, want %d — every line written while the disk was full must be there", len(rotated), len(want))
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("the fresh file cannot be created while the disk is full, Lstat err = %v", err)
	}
	if n := strings.Count(logs.String(), "appending through the rotated copy"); n != 2 {
		t.Errorf("each degraded open must be logged once, got %d lines:\n%s", n, logs.String())
	}
}

// captureLogOutput redirects the package-level default logger for the test's
// duration so the warning the rotation fallback emits is assertable.
func captureLogOutput(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}
