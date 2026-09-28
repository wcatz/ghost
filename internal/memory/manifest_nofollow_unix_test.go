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
// writeBackupManifest is a CLASSIFICATION, not the defence, and the difference is
// load-bearing. reserveBackupPath's create is O_EXCL, so its Lstat and its claim
// are one atomic step with no window between them; a manifest is replaced rather
// than refused, so it cannot be O_EXCL, and the enforcement is the O_NOFOLLOW on
// the open instead.
//
// This closes that window deliberately: the check is run first, so a link exists
// at the path by the time the create happens — which is the only state the flag
// has to catch, and the only one a pre-create Lstat alone would miss. Without the
// flag the open follows the link and the target is truncated, overwritten with a
// manifest and narrowed to 0600, so the assertions below are on the TARGET.
func TestWriteBackupManifestRefusesALinkPlantedAfterTheCheck(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "snapshot.db")
	if err := os.WriteFile(dest, []byte("bytes"), 0o600); err != nil {
		t.Fatalf("seed the snapshot: %v", err)
	}
	link := ManifestPath(dest)
	target := filepath.Join(dir, "important.json")

	// The link is created where the Lstat will look, and the file it points at is
	// something the user cares about. Its width is read rather than assumed: a
	// create applies the umask, and this process's is not necessarily 0.
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

	// Now the Lstat WOULD catch it — which is the point: this is the state the
	// O_EXCL-free path must still be safe in, and the only difference from the
	// test above is which of the two defences sees it.
	// info is printed, not dereferenced: this guard fires precisely when lerr
	// is non-nil, and then info is nil, so a Mode() call here panics in the one
	// case a reader needs the message for.
	if info, lerr := os.Lstat(link); lerr != nil || info.Mode().IsRegular() {
		t.Fatalf("the link is not in place (info %v, err %v), so this test would prove nothing", info, lerr)
	}

	// The create must refuse. It reaches the open with O_NOFOLLOW, so the kernel
	// refuses it however the link got there.
	f, err := os.OpenFile(link, manifestOpenFlags, 0o600)
	if err == nil {
		_ = f.Close()
		t.Fatal("the open followed a symlink at the manifest path")
	}
	if !errors.Is(err, syscall.ELOOP) {
		t.Errorf("open error = %v, want ELOOP: O_NOFOLLOW is what refuses this, and a different errno would mean a different cause", err)
	}

	body, err := os.ReadFile(target)
	if err != nil || string(body) != "do not lose me" {
		t.Errorf("the symlink target changed: %q (err %v)", body, err)
	}
	after, err := os.Stat(target)
	if err != nil {
		t.Fatalf("re-stat the target: %v", err)
	}
	if after.Mode().Perm() != before.Mode().Perm() {
		t.Errorf("the symlink target is now mode %04o, was %04o", after.Mode().Perm(), before.Mode().Perm())
	}
}
