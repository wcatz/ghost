//go:build !windows

package scratch

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestOwnerMarker_RefusesFIFO(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "entry")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, ownerFile), 0o600); err != nil {
		t.Fatal(err)
	}

	done := make(chan bool, 1)
	go func() {
		_, _, ok := ownerMarker(dir)
		done <- ok
	}()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("ownerMarker accepted a FIFO marker")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ownerMarker did not return within 3 seconds on a FIFO marker")
	}
}
