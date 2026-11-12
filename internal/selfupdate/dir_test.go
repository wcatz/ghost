package selfupdate

import (
	"path/filepath"
	"testing"
)

func TestDirOfAPathDirectlyUnderTheRootIsTheRoot(t *testing.T) {
	root := string(filepath.Separator)
	path := root + "ghost"
	if got := dirOf(path); got != root {
		t.Fatalf("dirOf(%q) = %q, want %q", path, got, root)
	}
	if joined := filepath.Join(dirOf(path), "ghost"); !filepath.IsAbs(joined) {
		t.Fatalf("filepath.Join(dirOf(%q), %q) = %q, want absolute", path, "ghost", joined)
	}
}
