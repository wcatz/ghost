//go:build !windows

package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/portable"
)

// TestRunImportCoreImportsFromANonSeekableArtifact: an artifact can arrive on a
// pipe or a FIFO, and neither can be rewound. The artifact is read once and the
// parsed records are what the run applies, so the source is never asked for a
// second pass.
func TestRunImportCoreImportsFromANonSeekableArtifact(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "artifact.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}

	body := `{"type":"header","schema_version":1}` + "\n" +
		`{"type":"project","project":{"id":"p1","path":"/src/p1","name":"one"}}` + "\n" +
		`{"type":"memory","memory":{"id":"m1","project_id":"p1","category":"fact","content":"from a pipe","source":"mcp"}}` + "\n"

	writeDone := make(chan error, 1)
	go func() {
		w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err != nil {
			writeDone <- err
			return
		}
		_, err = io.WriteString(w, body)
		if cerr := w.Close(); err == nil {
			err = cerr
		}
		writeDone <- err
	}()

	store := transferTestStore(t)
	var out strings.Builder
	err := runImportCore(context.Background(), store, fifo, portable.ImportOptions{Apply: true}, &out)

	select {
	case werr := <-writeDone:
		if werr != nil {
			t.Fatalf("write the artifact: %v", werr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the artifact writer did not finish")
	}

	if err != nil {
		t.Fatalf("runImportCore on a non-seekable artifact: %v", err)
	}
	if n := countMemories(t, store); n != 1 {
		t.Fatalf("imported %d memories from the FIFO, want 1\n%s", n, out.String())
	}
}
