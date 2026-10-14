package selfupdate

import (
	"path/filepath"
	"testing"
)

// TestDirOfAPathDirectlyUnderTheRootIsTheRoot pins the root case. A
// hand-rolled separator scan returned "" for "/ghost", so filepath.Join handed
// back a relative "ghost" and os.CreateTemp staged the update in os.TempDir()
// instead of beside the target — an EXDEV rename failure when /tmp is a
// separate filesystem from a binary installed at /ghost.
func TestDirOfAPathDirectlyUnderTheRootIsTheRoot(t *testing.T) {
	root := string(filepath.Separator)
	path := root + "ghost"

	got := dirOf(path)
	if got != root {
		t.Errorf("dirOf(%q) = %q, want %q", path, got, root)
	}
	// The documented guarantee is that a path derived from dirOf is absolute,
	// so that resolveSymlinks cannot fall back to the caller's working
	// directory and Replace stages beside the target.
	if joined := filepath.Join(dirOf(path), "ghost"); !filepath.IsAbs(joined) {
		t.Errorf("filepath.Join(dirOf(%q), %q) = %q, want an absolute path", path, "ghost", joined)
	}
}
