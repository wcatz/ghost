//go:build windows

package selfupdate

import (
	"path/filepath"
	"testing"
)

// TestDirOfAWindowsDriveRootedPath pins the Windows root case. A hand-rolled
// separator scan returned "C:" for "C:\ghost.exe", and filepath.Join("C:",
// "ghost.exe") is drive-relative by Go's own definition — so a staged update
// would land in the drive's current directory rather than beside the binary,
// and a relative symlink target at a drive root would resolve against it.
func TestDirOfAWindowsDriveRootedPath(t *testing.T) {
	got := dirOf(`C:\ghost.exe`)
	if want := `C:\`; got != want {
		t.Errorf(`dirOf(%q) = %q, want %q`, `C:\ghost.exe`, got, want)
	}
	if joined := filepath.Join(got, "ghost.exe"); !filepath.IsAbs(joined) {
		t.Errorf(`filepath.Join(%q, %q) = %q, want an absolute path`, got, "ghost.exe", joined)
	}
}
