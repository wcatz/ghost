//go:build !windows

package main

import (
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// TestRunBackupVerifyCoreReportsTheManifestFreeChecksForAFIFOManifest: a FIFO in
// the manifest's place is refused unopened, so the run reaches the checks that
// need no manifest instead of blocking until a writer appears.
func TestRunBackupVerifyCoreReportsTheManifestFreeChecksForAFIFOManifest(t *testing.T) {
	dest := backedUpSnapshot(t)
	path := memory.ManifestPath(dest)
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove the manifest: %v", err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	// Run on a goroutine: a regression that opens the FIFO blocks there, and the
	// deadline turns it into a failure instead of a hung suite. The writer below
	// is the unblock, so a hung read does not outlive the test either.
	type result struct {
		text string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		text, err := verifyReportOf(dest)
		done <- result{text, err}
	}()
	var r result
	select {
	case r = <-done:
	case <-time.After(5 * time.Second):
		if w, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = w.Close()
		}
		<-done
		t.Fatal("verify blocked reading a FIFO in the manifest's place")
	}
	requireUnreadableManifestReport(t, dest, r.text, r.err)
	if r.err != nil && !strings.Contains(r.err.Error(), "not a regular file") {
		t.Errorf("error = %v, want it to say the manifest is not a regular file:\n%s", r.err, r.text)
	}
}
