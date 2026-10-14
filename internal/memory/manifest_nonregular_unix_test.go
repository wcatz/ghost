//go:build !windows

package memory

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// verifyWithDeadline runs VerifyBackup on a goroutine so a regression that
// blocks in a read of a FIFO fails the test instead of hanging the suite.
func verifyWithDeadline(t *testing.T, dest string) (VerifyReport, error) {
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
		t.Fatal("VerifyBackup did not return: it is blocked reading a manifest that is not a regular file")
		return VerifyReport{}, nil
	}
}

func requireRefusedNonRegular(t *testing.T, dest string) {
	t.Helper()
	rep, err := verifyWithDeadline(t, dest)
	if err == nil {
		t.Fatalf("verify accepted a manifest that is not a regular file: %+v", rep)
	}
	if !rep.ManifestPresent {
		t.Error("ManifestPresent is false for a sidecar Lstat found on disk")
	}
	if rep.HasManifest {
		t.Error("HasManifest is true for a sidecar that could not be read")
	}
	if rep.ManifestErr == nil {
		t.Error("ManifestErr is nil, so the report would print the nil error it names")
	}
	if rep.BytesRead || rep.CountsRead {
		t.Errorf("a run that stopped at the manifest reports BytesRead=%v CountsRead=%v, want both false",
			rep.BytesRead, rep.CountsRead)
	}
}

func TestVerifyBackupRefusesAFIFOManifestWithoutBlocking(t *testing.T) {
	dest := backedUpFixture(t)
	path := ManifestPath(dest)
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove the manifest: %v", err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	requireRefusedNonRegular(t, dest)
}

func TestVerifyBackupRefusesASymlinkToAFIFOManifestWithoutBlocking(t *testing.T) {
	dest := backedUpFixture(t)
	path := ManifestPath(dest)
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove the manifest: %v", err)
	}
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	if err := os.Symlink(fifo, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	requireRefusedNonRegular(t, dest)
}

func TestVerifyBackupRefusesADirectoryManifest(t *testing.T) {
	dest := backedUpFixture(t)
	path := ManifestPath(dest)
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove the manifest: %v", err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	requireRefusedNonRegular(t, dest)
}

// A symlink to a regular file is followed, and says so in the docs.
func TestVerifyBackupFollowsASymlinkToAValidManifest(t *testing.T) {
	dest := backedUpFixture(t)
	path := ManifestPath(dest)
	real := filepath.Join(t.TempDir(), "real.manifest.json")
	if err := os.Rename(path, real); err != nil {
		t.Fatalf("move the manifest: %v", err)
	}
	if err := os.Symlink(real, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	rep, err := verifyWithDeadline(t, dest)
	if err != nil {
		t.Fatalf("verify refused a symlink to a valid manifest: %v", err)
	}
	if !rep.HasManifest {
		t.Error("HasManifest is false for a followed symlink to a valid manifest")
	}
}
