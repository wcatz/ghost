package selfupdate

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func skipSymlinkPerms(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "windows" {
		return
	}
	probe := filepath.Join(t.TempDir(), "probe")
	if err := os.Symlink(probe, probe+".lnk"); err != nil {
		t.Skipf("symlinks not permitted on this host: %v", err)
	}
}

func TestResolveSymlinksRelativeTargetIsResolvedAgainstTheLinkDirectory(t *testing.T) {
	skipSymlinkPerms(t)

	base := t.TempDir()
	libDir := filepath.Join(base, "lib")
	binDir := filepath.Join(base, "bin")
	for _, d := range []string{libDir, binDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	real := filepath.Join(libDir, "ghost")
	if err := os.WriteFile(real, []byte("real"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(binDir, "ghost")
	if err := os.Symlink(filepath.Join("..", "lib", "ghost"), link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	t.Chdir(t.TempDir())

	got, err := resolveSymlinks(link)
	if err != nil {
		t.Fatalf("resolveSymlinks: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("resolveSymlinks returned a non-absolute path %q; a relative result is interpreted against the caller's working directory", got)
	}
	if filepath.Clean(got) != filepath.Clean(real) {
		t.Errorf("resolveSymlinks(%q) = %q, want %q", link, got, real)
	}
}

func TestResolveSymlinksFollowsChains(t *testing.T) {
	skipSymlinkPerms(t)

	base := t.TempDir()
	real := filepath.Join(base, "ghost-1.0")
	if err := os.WriteFile(real, []byte("real"), 0o755); err != nil {
		t.Fatal(err)
	}
	middle := filepath.Join(base, "ghost")
	if err := os.Symlink(real, middle); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	link := filepath.Join(base, "current")
	if err := os.Symlink(middle, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	got, err := resolveSymlinks(link)
	if err != nil {
		t.Fatalf("resolveSymlinks: %v", err)
	}
	if filepath.Clean(got) != filepath.Clean(real) {
		t.Errorf("resolveSymlinks(%q) = %q, want the end of the chain %q", link, got, real)
	}
}

func TestReplaceThroughRelativeSymlinkWritesToRealTarget(t *testing.T) {
	skipSymlinkPerms(t)

	base := t.TempDir()
	libDir := filepath.Join(base, "lib")
	binDir := filepath.Join(base, "bin")
	for _, d := range []string{libDir, binDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	elsewhere := t.TempDir()

	real := filepath.Join(libDir, "ghost")
	if err := os.WriteFile(real, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(binDir, "ghost")
	if err := os.Symlink(filepath.Join("..", "lib", "ghost"), link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	t.Chdir(elsewhere)

	if _, err := Replace(link, []byte("new-binary")); err != nil {
		t.Fatalf("Replace through a relative symlink: %v", err)
	}

	got, err := os.ReadFile(real)
	if err != nil {
		t.Fatalf("reading update target: %v", err)
	}
	if string(got) != "new-binary" {
		t.Errorf("real binary now holds %q, want %q — Replace wrote somewhere other than the symlink's target", got, "new-binary")
	}

	if target, err := os.Readlink(link); err != nil {
		t.Errorf("expected %q to remain a symlink after Replace, got: %v", link, err)
	} else if filepath.Clean(filepath.Join(binDir, target)) != filepath.Clean(real) {
		t.Errorf("symlink now points at %q, want %q", target, real)
	}

	entries, err := os.ReadDir(elsewhere)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("Replace left files in the working directory %q: %s", elsewhere, strings.Join(names, ", "))
	}
}
