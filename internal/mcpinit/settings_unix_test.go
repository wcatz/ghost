//go:build unix

package mcpinit

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestWriteFileAtomicReplacesReadOnlyFile pins that a config the user made
// read-only is still repaired, and keeps its mode: the replacement is written
// through a writable temp file (0600) and only then given the target's exact
// permissions, the same shape the settings.json write has always had. Windows
// is excluded because there a read-only file cannot be replaced at all, by
// rename or by os.WriteFile, so there is no contract to pin.
func TestWriteFileAtomicReplacesReadOnlyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("a = 1\n"), 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}

	if err := writeFileAtomic(path, []byte("b = 2\n"), 0644); err != nil {
		t.Fatalf("writeFileAtomic over a read-only file: %v", err)
	}
	assertFileContent(t, path, "b = 2\n")
	assertFileMode(t, path, 0400)
}

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

// TestSettingsFile_SaveNeverWidensMode pins that a file able to hold
// credentials cannot end up more permissive than 0600, even when the user's
// copy was group- or world-readable. Before the atomic-write refactor every
// save chmod'd the file to 0600, so keeping a 0644 settings.json readable by
// others was a regression. Unix only: the 0600 ceiling is a permission-bit
// contract, and on Windows os.Chmod(0400) sets FILE_ATTRIBUTE_READONLY, which
// then blocks the rename that replaces the file at all.
func TestSettingsFile_SaveNeverWidensMode(t *testing.T) {
	cases := map[string]os.FileMode{
		"world-readable copy": 0644,
		"group-readable copy": 0640,
		"already private":     0600,
		"narrower than 0600":  0400,
	}
	for name, mode := range cases {
		t.Run(name, func(t *testing.T) {
			path := tempSettings(t, `{"effortLevel":"high"}`)
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			sf, err := loadSettings(path)
			if err != nil {
				t.Fatalf("loadSettings: %v", err)
			}
			if err := sf.save(); err != nil {
				t.Fatalf("save: %v", err)
			}
			assertFileMode(t, path, mode&0600)
		})
	}
}
