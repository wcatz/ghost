//go:build !windows

package memory

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestWriteBackupManifestRefusesALinkPlantedAfterTheCheck: the Lstat in
// writeBackupManifest is a classification, not the defence; a manifest is
// replaced rather than refused, so it cannot be O_EXCL and the enforcement is
// O_NOFOLLOW on the open. With a link in place the open must fail with ELOOP and
// leave the target's content and mode alone.
func TestWriteBackupManifestRefusesALinkPlantedAfterTheCheck(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "snapshot.db")
	if err := os.WriteFile(dest, []byte("bytes"), 0o600); err != nil {
		t.Fatalf("seed the snapshot: %v", err)
	}
	link := ManifestPath(dest)
	target := filepath.Join(dir, "important.json")
	if err := os.WriteFile(target, []byte("do not lose me"), 0o644); err != nil {
		t.Fatalf("seed the target: %v", err)
	}
	before, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat the target: %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	f, err := os.OpenFile(link, manifestOpenFlags, 0o600)
	if err == nil {
		_ = f.Close()
		t.Fatal("the open followed a symlink at the manifest path")
	}
	if !errors.Is(err, syscall.ELOOP) {
		t.Errorf("open error = %v, want ELOOP", err)
	}
	body, err := os.ReadFile(target)
	if err != nil || string(body) != "do not lose me" {
		t.Errorf("the symlink target changed: %q (err %v)", body, err)
	}
	if after, err := os.Stat(target); err != nil || after.Mode().Perm() != before.Mode().Perm() {
		t.Errorf("the symlink target is now mode %v (err %v), was %v", after, err, before.Mode().Perm())
	}
}
