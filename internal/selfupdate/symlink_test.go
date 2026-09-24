package selfupdate

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// skipSymlinkPerms reports whether this platform/user can create symlinks at
// all. Windows denies CreateSymbolicLink without Developer Mode or
// SeCreateSymbolicLinkPrivilege, and the test suite runs there, so a hard
// failure would be a false positive rather than a real regression.
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

// TestResolveSymlinksRelativeTargetIsResolvedAgainstTheLinkDirectory pins the
// core defect: os.Readlink returns a relative target verbatim, and that
// target is only meaningful relative to the directory holding the symlink —
// not the process working directory. Self-update runs from wherever the user
// invoked `ghost upgrade`, which is essentially never the symlink's directory.
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
	// "../lib/ghost" is only correct relative to binDir.
	if err := os.Symlink(filepath.Join("..", "lib", "ghost"), link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	// Run from a directory that is NOT the symlink's directory, so a
	// working-directory-relative interpretation cannot accidentally work.
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

// TestResolveSymlinksFollowsChains covers a symlink pointing at another
// symlink. A single os.Readlink stops at the first hop and hands back another
// symlink path, so Replace would rename over a symlink instead of the binary.
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

// TestResolveSymlinksPlainFilePassesThrough is the non-symlink path: a
// regular binary must come back untouched so Replace keeps working for the
// overwhelmingly common install layout.
func TestResolveSymlinksPlainFilePassesThrough(t *testing.T) {
	plain := filepath.Join(t.TempDir(), "ghost")
	if err := os.WriteFile(plain, []byte("real"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := resolveSymlinks(plain)
	if err != nil {
		t.Fatalf("resolveSymlinks: %v", err)
	}
	if got != plain {
		t.Errorf("resolveSymlinks(%q) = %q, want the path unchanged", plain, got)
	}
}

// TestResolveSymlinksDanglingTargetErrors makes a broken link fail loudly.
// Silently returning the dangling path defers the failure to os.Stat with a
// message that names the wrong file, which is how an upgrade gets reported as
// "stat ../lib/ghost: no such file" with no indication a symlink was involved.
func TestResolveSymlinksDanglingTargetErrors(t *testing.T) {
	skipSymlinkPerms(t)

	base := t.TempDir()
	link := filepath.Join(base, "ghost")
	if err := os.Symlink(filepath.Join(base, "does-not-exist"), link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	got, err := resolveSymlinks(link)
	if err == nil {
		t.Fatalf("resolveSymlinks on a dangling link returned %q with no error; a broken symlink must not resolve successfully", got)
	}
}

// TestReplaceThroughRelativeSymlinkWritesToRealTarget exercises the user
// visible failure, not just the helper. Before the fix, Replace computed the
// temp-file directory from the unresolved relative target, so the rename
// landed against the working directory and either failed or replaced the
// wrong file.
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

	// The working directory must be a *sibling* of base, not nested inside
	// it. Nesting would make "../lib/ghost" walk back into base/lib by
	// accident and let a working-directory-relative resolution pass the test
	// while still being wrong for any real install layout.
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

	if err := Replace(link, []byte("new-binary")); err != nil {
		t.Fatalf("Replace through a relative symlink: %v", err)
	}

	got, err := os.ReadFile(real)
	if err != nil {
		t.Fatalf("reading update target: %v", err)
	}
	if string(got) != "new-binary" {
		t.Errorf("real binary now holds %q, want %q — Replace wrote somewhere other than the symlink's target", got, "new-binary")
	}

	// The link itself must still be a link, not be clobbered into a file.
	if target, err := os.Readlink(link); err != nil {
		t.Errorf("expected %q to remain a symlink after Replace, got: %v", link, err)
	} else if filepath.Clean(filepath.Join(binDir, target)) != filepath.Clean(real) {
		t.Errorf("symlink now points at %q, want %q", target, real)
	}

	// Nothing may be written into the working directory: a relative temp dir
	// would drop the update here, outside the install prefix.
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

// TestReplaceLeavesNoTempFilesBehind guards the failure path cleanup: if the
// rename fails, the partial temp file must not be left next to the binary.
func TestReplaceLeavesNoTempFilesBehind(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "ghost")
	if err := os.WriteFile(plain, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := Replace(plain, []byte("new")); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %d entries after Replace, want 1: %s", len(entries), strings.Join(names, ", "))
	}
}
