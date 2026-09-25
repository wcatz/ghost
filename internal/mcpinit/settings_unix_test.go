//go:build !windows

package mcpinit

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestWriteFileAtomicAppliesUmaskOnCreate pins that a newly created user config
// is created with perm subject to the process umask, exactly as os.WriteFile
// does. A temp file created at a fixed mode and then chmod-ed would bypass the
// umask, leaving a 0644 config.toml world-readable on a host with a hardened
// umask (077), even though it can carry [mcp_servers.*.env] values.
func TestWriteFileAtomicAppliesUmaskOnCreate(t *testing.T) {
	restore := syscall.Umask(0o077)
	defer syscall.Umask(restore)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := writeFileAtomic(path, []byte("a = 1\n"), 0644); err != nil {
		t.Fatalf("writeFileAtomic (create): %v", err)
	}
	assertFileContent(t, path, "a = 1\n")
	assertFileMode(t, path, 0600)

	// Replacing an existing file keeps that file's exact mode; the umask must
	// not narrow it either.
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("b = 2\n"), 0644); err != nil {
		t.Fatalf("writeFileAtomic (replace): %v", err)
	}
	assertFileMode(t, path, 0644)

	// A permissive umask leaves the requested mode alone.
	permissive := syscall.Umask(0o022)
	other := filepath.Join(dir, "fresh.toml")
	if err := writeFileAtomic(other, []byte("c = 3\n"), 0644); err != nil {
		t.Fatalf("writeFileAtomic (create, umask 022): %v", err)
	}
	syscall.Umask(permissive)
	assertFileMode(t, other, 0644)
}
