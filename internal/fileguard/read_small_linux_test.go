//go:build linux

package fileguard

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestReadSmallRegularFileDoesNotBlockOnFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pid")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := ReadSmallRegularFile(path, 4096)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO read as a regular file")
		}
	case <-time.After(time.Second):
		t.Fatal("ReadSmallRegularFile blocked on a FIFO")
	}
}
