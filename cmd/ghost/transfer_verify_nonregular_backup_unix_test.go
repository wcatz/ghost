//go:build !windows

package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestRunBackupVerifyCoreRefusesAFIFOBackup: a FIFO named as the backup is
// refused unopened, with the refusing verdict and an error naming the type, so
// the command exits 1 instead of waiting for a writer.
func TestRunBackupVerifyCoreRefusesAFIFOBackup(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "snap.db")
	if err := syscall.Mkfifo(dest, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
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
		// Keep opening and closing a writer until the stuck call returns.
		for stuck := true; stuck; {
			if f, err := os.OpenFile(dest, os.O_RDWR|syscall.O_NONBLOCK, 0); err == nil {
				_ = f.Close()
			}
			select {
			case <-done:
				stuck = false
			case <-time.After(50 * time.Millisecond):
			}
		}
		t.Fatal("verify blocked on a FIFO named as the backup")
	}
	if r.err == nil {
		t.Fatalf("verify accepted a FIFO as the backup:\n%s", r.text)
	}
	if !strings.Contains(r.err.Error(), "not a regular file") || !strings.Contains(r.err.Error(), dest) {
		t.Errorf("error = %v, want it to name %s as not a regular file", r.err, dest)
	}
	if want := "(" + fs.ModeNamedPipe.String() + ")"; !strings.Contains(r.err.Error(), want) {
		t.Errorf("error = %v, want it to name the file type %s", r.err, want)
	}
	headline := strings.SplitN(r.text, "\n", 2)[0]
	if want := "refusing " + dest + ", not checked"; headline != want {
		t.Errorf("headline = %q, want %q", headline, want)
	}
}
