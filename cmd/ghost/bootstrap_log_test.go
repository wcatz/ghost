package main

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/wcatz/ghost/internal/mcpinit"
)

// seedLogAtCap writes exactly mcpinit.LogRotateCap bytes to path — a marker
// first line, then padding — and returns the whole content, so a test can
// prove the file that was there is the one that was rotated away.
func seedLogAtCap(t *testing.T, path string) []byte {
	t.Helper()
	body := []byte("the oversized log\n")
	pad := bytes.Repeat([]byte("x"), mcpinit.LogRotateCap-len(body))
	content := append(body, pad...)
	if len(content) != mcpinit.LogRotateCap {
		t.Fatalf("seeded %d bytes, want exactly the cap %d", len(content), mcpinit.LogRotateCap)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("seed oversized log: %v", err)
	}
	return content
}

// TestMCPLogConfig_RotatesAnOversizedFileAtTheCap guards the issue's size half:
// the MCP server appends for a whole client session, and with GHOST_DEBUG that
// stream is the full debug log, so the redirect must obey the same cap every
// other Ghost log does. An existing file at the cap is rotated to "<path>.1",
// replacing any previous copy, and this run appends to a fresh file.
func TestMCPLogConfig_RotatesAnOversizedFileAtTheCap(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "mcp.log")
	content := seedLogAtCap(t, logPath)
	if err := os.WriteFile(logPath+".1", []byte("a previous rotation"), 0o600); err != nil {
		t.Fatalf("seed stale .1: %v", err)
	}
	t.Setenv("GHOST_LOG_FILE", logPath)
	t.Setenv("GHOST_DEBUG", "")

	writer, level, closeLog := mcpLogConfig(false)
	if closeLog == nil {
		t.Fatal("GHOST_LOG_FILE was opened, closer must be non-nil")
	}
	logger := slog.New(slog.NewTextHandler(writer, &slog.HandlerOptions{Level: level}))
	logger.Info("this run's line")
	closeLog()

	// The rotated copy is the proof rotation happened, and the assertion is by
	// length and head rather than by dumping the file: a failure here would
	// otherwise print five megabytes of padding into the test log.
	rotated, err := os.ReadFile(logPath + ".1")
	if err != nil {
		t.Fatalf("read rotated copy: %v", err)
	}
	if !bytes.Equal(rotated, content) {
		t.Errorf(".1 holds %d bytes starting %q, want the %d-byte oversized file rotated there",
			len(rotated), headBytes(rotated, 40), len(content))
	}
	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(got), "this run's line") {
		t.Errorf("fresh log does not carry this run's line, got %d bytes starting %q", len(got), headBytes(got, 40))
	}
}

// TestMCPLogConfig_CreatesTheFilePrivate guards the issue's mode half: a
// client-spawned server's log is readable by every local account if it is
// created at the umask default. Every comparable Ghost file is 0600, and the
// redirect must be too, whatever the umask.
func TestMCPLogConfig_CreatesTheFilePrivate(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "mcp.log")
	t.Setenv("GHOST_LOG_FILE", logPath)
	t.Setenv("GHOST_DEBUG", "")

	_, _, closeLog := mcpLogConfig(false)
	if closeLog == nil {
		t.Fatal("GHOST_LOG_FILE was opened, closer must be non-nil")
	}
	closeLog()

	fi, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat log: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %04o, want 0600: the server log must be owner-only whatever the umask", got)
	}
}

// headBytes returns the first n bytes of b as a string, for a failure message
// that describes an oversized file instead of printing it whole.
func headBytes(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
	}
	return string(b)
}

// writeLines writes n lines of lineLen bytes (newline included) through w and
// fails the test if any write reports an error or a short count.
func writeLines(t *testing.T, w io.Writer, n, lineLen int) {
	t.Helper()
	line := append(bytes.Repeat([]byte("l"), lineLen-1), '\n')
	for i := 0; i < n; i++ {
		if k, err := w.Write(line); err != nil || k != len(line) {
			t.Fatalf("write %d: n=%d err=%v, a log write must never fail its caller", i, k, err)
		}
	}
}

// TestMCPLogConfig_HoldsTheCapWhileTheServerRuns guards #1546: the redirect is
// opened once and kept for the whole run, so the open-time cap alone let it
// grow without bound. Seven mebibytes written through the returned writer must
// leave the live file within the cap plus one line, a ".1" beside it, and both
// owner-only.
func TestMCPLogConfig_HoldsTheCapWhileTheServerRuns(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "mcp.log")
	t.Setenv("GHOST_LOG_FILE", logPath)
	t.Setenv("GHOST_DEBUG", "")

	writer, _, closeLog := mcpLogConfig(false)
	if closeLog == nil {
		t.Fatal("GHOST_LOG_FILE was opened, closer must be non-nil")
	}
	const lineLen = 1024
	const lines = 7 << 10 // 7 MiB
	writeLines(t, writer, lines, lineLen)
	closeLog()

	live, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat live log: %v", err)
	}
	if live.Size() > mcpinit.LogRotateCap+lineLen {
		t.Errorf("live log is %d bytes, want at most the cap %d plus one line %d",
			live.Size(), mcpinit.LogRotateCap, lineLen)
	}
	rotated, err := os.Stat(logPath + ".1")
	if err != nil {
		t.Fatalf("no .1 after writing past the cap: %v", err)
	}
	for _, fi := range []os.FileInfo{live, rotated} {
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %04o, want 0600", fi.Name(), got)
		}
	}
	// One rotation, so nothing may be lost between the two files.
	if got := live.Size() + rotated.Size(); got != lines*lineLen {
		t.Errorf("live+.1 = %d bytes, want all %d written", got, lines*lineLen)
	}
}

// TestMCPLogConfig_BelowTheCapDoesNotRotate is the control for the test above:
// the rotation must key on the cap, not fire on every write.
func TestMCPLogConfig_BelowTheCapDoesNotRotate(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "mcp.log")
	t.Setenv("GHOST_LOG_FILE", logPath)
	t.Setenv("GHOST_DEBUG", "")

	writer, _, closeLog := mcpLogConfig(false)
	writeLines(t, writer, 1<<10, 1024) // 1 MiB
	closeLog()

	if _, err := os.Stat(logPath + ".1"); !os.IsNotExist(err) {
		t.Errorf(".1 stat err = %v, want not-exist below the cap", err)
	}
	fi, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat log: %v", err)
	}
	if fi.Size() != 1<<20 {
		t.Errorf("log is %d bytes, want %d", fi.Size(), 1<<20)
	}
}

// TestMCPLogConfig_ConcurrentWritersRotateSafely is for -race: slog handlers
// write from many goroutines, and a rotation closes and reopens the shared
// descriptor under them. No write may fail and no byte may be lost.
func TestMCPLogConfig_ConcurrentWritersRotateSafely(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "mcp.log")
	t.Setenv("GHOST_LOG_FILE", logPath)
	t.Setenv("GHOST_DEBUG", "")

	writer, _, closeLog := mcpLogConfig(false)
	const goroutines, perG, lineLen = 8, 800, 1024 // 6.25 MiB
	line := append(bytes.Repeat([]byte("c"), lineLen-1), '\n')
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				if _, err := writer.Write(line); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	closeLog()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent write failed: %v", err)
	}

	live, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat live log: %v", err)
	}
	rotated, err := os.Stat(logPath + ".1")
	if err != nil {
		t.Fatalf("no .1 after writing past the cap: %v", err)
	}
	if live.Size() > mcpinit.LogRotateCap+lineLen {
		t.Errorf("live log is %d bytes, want at most cap plus one line", live.Size())
	}
	if got := live.Size() + rotated.Size(); got != goroutines*perG*lineLen {
		t.Errorf("live+.1 = %d bytes, want %d", got, goroutines*perG*lineLen)
	}
}
