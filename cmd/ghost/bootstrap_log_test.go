package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
