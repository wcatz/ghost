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

// TestVerifyBackupRefusesANonRegularManifestWithoutBlocking: a FIFO, a symlink to
// a FIFO or a directory in the manifest's place is refused unopened (never
// blocking), reported as present-but-unreadable, and the checks that need no
// manifest still run. A symlink to a regular manifest is followed.
func TestVerifyBackupRefusesANonRegularManifestWithoutBlocking(t *testing.T) {
	for _, tc := range []struct {
		name  string
		plant func(t *testing.T, path string)
	}{
		{"fifo", func(t *testing.T, path string) {
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Skipf("mkfifo unavailable: %v", err)
			}
		}},
		{"symlink to fifo", func(t *testing.T, path string) {
			fifo := filepath.Join(t.TempDir(), "fifo")
			if err := syscall.Mkfifo(fifo, 0o600); err != nil {
				t.Skipf("mkfifo unavailable: %v", err)
			}
			if err := os.Symlink(fifo, path); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
		}},
		{"directory", func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dest := backedUpFixture(t)
			path := ManifestPath(dest)
			if err := os.Remove(path); err != nil {
				t.Fatalf("remove the manifest: %v", err)
			}
			tc.plant(t, path)
			rep, err := verifyWithDeadline(t, dest)
			if err == nil {
				t.Fatalf("verify accepted a manifest that is not a regular file: %+v", rep)
			}
			if !rep.ManifestPresent || rep.HasManifest || rep.ManifestErr == nil {
				t.Errorf("ManifestPresent=%v HasManifest=%v ManifestErr=%v, want present but unreadable", rep.ManifestPresent, rep.HasManifest, rep.ManifestErr)
			}
			requireManifestFreeChecksRan(t, dest, rep, err)
		})
	}

	t.Run("symlink to a valid manifest is followed", func(t *testing.T) {
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
		if err != nil || !rep.HasManifest {
			t.Fatalf("verify refused a symlink to a valid manifest: HasManifest=%v err=%v", rep.HasManifest, err)
		}
	})
}
