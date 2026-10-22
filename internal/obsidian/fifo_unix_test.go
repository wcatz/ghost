//go:build !windows

package obsidian

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestWriteIfChangedRefusesFIFO: a FIFO at a note path must be refused
// unopened rather than read, so the call returns instead of blocking.
func TestWriteIfChangedRefusesFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "note.md")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := writeIfChanged(path, "content")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("writeIfChanged accepted a FIFO as a note path")
		}
		if !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("error = %v, want it to say the path is not a regular file", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("writeIfChanged blocked on a FIFO named as the note path")
	}
	// The FIFO is left in place; the caller decides what to do with it.
	if fi, err := os.Lstat(path); err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("FIFO should remain untouched, got %v", err)
	}
}

// TestPruneLeavesFIFOMarkdownAlone: a FIFO carrying the Markdown suffix
// inside a managed project folder is skipped, not read, so prune does not
// block and leaves it in place.
func TestPruneLeavesFIFOMarkdownAlone(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, markerName), []byte(`{"schema_version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "proj", "Memories")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(sub, "blocked.md")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- prune(root, []string{"proj"}, keepSet{}, nil) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("prune: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("prune blocked on a FIFO carrying the Markdown suffix")
	}
	if _, err := os.Lstat(fifo); err != nil {
		t.Errorf("prune removed a FIFO it never identified as a note: %v", err)
	}
}

// TestHasGhostIDOnFIFO: hasGhostID on a FIFO returns false without blocking.
func TestHasGhostIDOnFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "note.md")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}

	done := make(chan struct{}, 1)
	go func() {
		id, ok := hasGhostID(path)
		if ok {
			t.Errorf("hasGhostID(FIFO) = %q, true; want false", id)
		}
		done <- struct{}{}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("hasGhostID blocked on a FIFO")
	}
}

// TestHasGhostContentWithOnlyFIFO: hasGhostContent on a directory containing
// only a FIFO .md returns false without blocking.
func TestHasGhostContentWithOnlyFIFO(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "blocked.md")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}

	done := make(chan struct{}, 1)
	go func() {
		found := hasGhostContent(dir)
		if found {
			t.Error("hasGhostContent(FIFO dir) = true; want false")
		}
		done <- struct{}{}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("hasGhostContent blocked on a directory with only a FIFO .md")
	}
}
