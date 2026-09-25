//go:build linux

package maintenance

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestReapStaleProcessFilesDoesNotReadFIFO(t *testing.T) {
	noOpenProbe(t)
	dir := t.TempDir()
	fifo := filepath.Join(dir, "reflect-block.pid")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := ReapStaleProcessFiles(dir)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ReapStaleProcessFiles: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("retention blocked reading a legacy PID FIFO")
	}
	if _, err := os.Stat(fifo); err != nil {
		t.Fatalf("non-regular PID-shaped file was removed: %v", err)
	}
}
