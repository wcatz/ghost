//go:build !windows

package memory

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// verifyBackupPathWithDeadline runs VerifyBackup on a goroutine so a regression
// that blocks reading a FIFO as the backup itself fails the test instead of
// hanging the suite. On a timeout it repeatedly opens and closes a writer on the
// FIFO until VerifyBackup returns, so a stuck open or read ends with end of
// file and the goroutine does not outlive the test.
func verifyBackupPathWithDeadline(t *testing.T, dest, fifo string) (VerifyReport, error) {
	t.Helper()
	type result struct {
		rep VerifyReport
		err error
	}
	done := make(chan result, 1)
	go func() {
		rep, err := VerifyBackup(context.Background(), dest)
		done <- result{rep, err}
	}()
	select {
	case r := <-done:
		return r.rep, r.err
	case <-time.After(5 * time.Second):
		if fifo != "" {
			// Keep opening and closing a writer until the stuck call returns.
			for stuck := true; stuck; {
				if f, err := os.OpenFile(fifo, os.O_RDWR|syscall.O_NONBLOCK, 0); err == nil {
					_ = f.Close()
				}
				select {
				case <-done:
					stuck = false
				case <-time.After(50 * time.Millisecond):
				}
			}
		}
		t.Fatal("VerifyBackup did not return: it is blocked on a backup path that is not a regular file")
		return VerifyReport{}, nil
	}
}

func requireBackupPathRefused(t *testing.T, dest, fifo, typeWord string) {
	t.Helper()
	rep, err := verifyBackupPathWithDeadline(t, dest, fifo)
	if err == nil {
		t.Fatalf("verify accepted a backup path that is not a regular file: %+v", rep)
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("error = %v, want it to say the backup is not a regular file", err)
	}
	if !strings.Contains(err.Error(), typeWord) {
		t.Errorf("error = %v, want it to name the file type (%q)", err, typeWord)
	}
	if rep.BytesRead {
		t.Error("BytesRead is true: the report states a size for a file it never checked")
	}
	if len(rep.Checks) != 0 {
		t.Errorf("checks ran against a backup that was refused unopened: %+v", rep.Checks)
	}
}

func TestVerifyBackupRefusesAFIFOBackupWithoutBlocking(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "snap.db")
	if err := syscall.Mkfifo(dest, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	requireBackupPathRefused(t, dest, dest, "("+fs.ModeNamedPipe.String()+")")
}

func TestVerifyBackupRefusesAFIFOBackupBesideAManifestWithoutBlocking(t *testing.T) {
	dest := backedUpFixture(t)
	if err := os.Remove(dest); err != nil {
		t.Fatalf("remove the backup: %v", err)
	}
	if err := syscall.Mkfifo(dest, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	if _, err := os.Lstat(ManifestPath(dest)); err != nil {
		t.Fatalf("the fixture has no manifest beside it: %v", err)
	}
	requireBackupPathRefused(t, dest, dest, "("+fs.ModeNamedPipe.String()+")")
}

func TestVerifyBackupRefusesADirectoryBackup(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "snap.db")
	if err := os.Mkdir(dest, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	requireBackupPathRefused(t, dest, "", "("+fs.ModeDir.String()+")")
}

func TestVerifyBackupRefusesASymlinkToAFIFOBackupWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	dest := filepath.Join(dir, "snap.db")
	if err := os.Symlink(fifo, dest); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	requireBackupPathRefused(t, dest, fifo, "("+fs.ModeNamedPipe.String()+")")
}

// A symlink to a regular backup is still followed.
func TestVerifyBackupFollowsASymlinkToARegularBackup(t *testing.T) {
	dest := backedUpFixture(t)
	link := filepath.Join(t.TempDir(), "link.db")
	if err := os.Symlink(dest, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	rep, err := verifyBackupPathWithDeadline(t, link, "")
	if err != nil {
		t.Fatalf("verify refused a symlink to a regular backup: %v", err)
	}
	if !rep.BytesRead {
		t.Error("BytesRead is false for a followed symlink to a regular backup")
	}
}
