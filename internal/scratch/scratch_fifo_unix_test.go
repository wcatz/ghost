//go:build !windows

package scratch

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// mkfifo creates a FIFO at path. Test helper.
func mkfifo(path string, mode uint32) error {
	return syscall.Mkfifo(path, mode)
}

func TestOwnerMarker_RefusesFIFO(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "entry")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, ownerFile)
	if err := mkfifo(marker, 0o600); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct {
		pid   int
		start string
		ok    bool
	}, 1)
	go func() {
		pid, start, ok := ownerMarker(dir)
		done <- struct {
			pid   int
			start string
			ok    bool
		}{pid, start, ok}
	}()
	select {
	case res := <-done:
		if res.ok {
			t.Fatalf("ownerMarker accepted a FIFO marker: pid=%d start=%q", res.pid, res.start)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ownerMarker did not return within 3 seconds on a FIFO marker")
	}
}
