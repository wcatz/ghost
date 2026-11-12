package main

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/wcatz/ghost/internal/mcpinit"
)

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
