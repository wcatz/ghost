package memory

import (
	"os"
	"path/filepath"
	"testing"
)

// The shared data-directory fixture, deliberately UNTAGGED.
//
// It was defined in perms_test.go, which is //go:build !windows because everything
// in that file asserts POSIX file modes. The retrieval key's own tests need the
// same fixture, and they are not a POSIX-modes suite: the key file's atomic-publish
// path, its race handling and its refusal behaviour are all platform-independent,
// and `internal/mcpserver`'s equivalent fixture is untagged too. Leaving the
// helper in a !windows file broke the Windows build of this package with
// `undefined: fakeDataDir` — which is the repository's own `go vet ./...` rule, on
// a platform this repo ships binaries for and has CI legs for.
//
// The MODE assertion the key tests make is the one genuinely POSIX-only claim in
// them, so it is skipped there by name rather than taking the whole file with it.

// fakeDataDir points config.DataDirPath at a temp tree and returns that tree's
// ghost subdirectory, standing in for ~/.local/share/ghost. Without this the tests
// would tighten — or, for the key, CREATE A FILE IN — the developer's real data
// directory, which is the failure this fixture exists to prevent.
func fakeDataDir(t *testing.T) string {
	t.Helper()
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	dir := filepath.Join(dataHome, "ghost")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	return dir
}
