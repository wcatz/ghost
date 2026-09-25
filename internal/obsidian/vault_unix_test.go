//go:build !windows

package obsidian

import (
	"os"
	"path/filepath"
	"testing"
)

// TestVaultIsNotWorldReadable: the vault holds whatever the mirror holds and
// was written 0755/0644 — readable by every account on the machine. On
// Windows os.Stat reports 0666/0777 regardless of ACLs, so the equivalent
// guarantee is asserted against the DACL in vault_windows_test.go.
func TestVaultIsNotWorldReadable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault")
	if err := ensureVault(root); err != nil {
		t.Fatalf("ensureVault: %v", err)
	}
	if st, err := os.Stat(root); err != nil {
		t.Fatalf("stat vault: %v", err)
	} else if st.Mode().Perm() != 0o700 {
		t.Errorf("vault dir mode = %04o, want 0700", st.Mode().Perm())
	}
	if st, err := os.Stat(filepath.Join(root, markerName)); err != nil {
		t.Fatalf("stat marker: %v", err)
	} else if st.Mode().Perm() != 0o600 {
		t.Errorf("marker mode = %04o, want 0600", st.Mode().Perm())
	}

	// A written note exercises both the nested directory and the file itself.
	note := filepath.Join(root, "proj", "Notes", "n.md")
	if _, err := writeIfChanged(note, "hello\n"); err != nil {
		t.Fatalf("writeIfChanged: %v", err)
	}
	// Chmod sets the mode outright, bypassing umask — so this assertion
	// fails deterministically whatever the environment's umask happens to be.
	if st, err := os.Stat(note); err != nil {
		t.Fatalf("stat note: %v", err)
	} else if st.Mode().Perm() != 0o600 {
		t.Errorf("note mode = %04o, want 0600", st.Mode().Perm())
	}
	if st, err := os.Stat(filepath.Dir(note)); err != nil {
		t.Fatalf("stat note dir: %v", err)
	} else if st.Mode().Perm() != 0o700 {
		t.Errorf("note dir mode = %04o, want 0700", st.Mode().Perm())
	}
}

// TestEnsureVaultTightensLegacyPermissions: 0700/0600 only reaches files
// Ghost writes after the fact — writeIfChanged skips unchanged content and
// MkdirAll never retightens an existing directory — so a vault created
// before the permission fix stays 0755/0644 forever. ensureVault is the
// one place that sees the whole vault on startup; when the marker says the
// vault is Ghost's, it must tighten what Ghost owns and leave user files'
// modes alone.
func TestEnsureVaultTightensLegacyPermissions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault")
	proj := filepath.Join(root, "proj")
	memories := filepath.Join(proj, "Memories")
	mustMkdirAll(t, memories)
	marker := filepath.Join(root, markerName)
	mustWrite(t, marker, `{"schema_version":1}`+"\n")
	note := filepath.Join(memories, "n.md")
	mustWrite(t, note, ghostNote)
	own := filepath.Join(proj, "own.md") // no frontmatter — the user's file
	mustWrite(t, own, userNote)

	// Chmod the legacy modes outright: relying on umask would let the
	// environment hand us 0700/0600 for free and the assertions would pass
	// without the fix.
	for _, d := range []string{root, proj, memories} {
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{marker, note, own} {
		if err := os.Chmod(f, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := ensureVault(root); err != nil {
		t.Fatalf("ensureVault: %v", err)
	}

	for _, tc := range []struct {
		path string
		want os.FileMode
	}{
		{root, 0o700},
		{proj, 0o700},
		{memories, 0o700},
		{marker, 0o600},
		{note, 0o600},
	} {
		st, err := os.Stat(tc.path)
		if err != nil {
			t.Fatalf("stat %s: %v", tc.path, err)
		}
		if st.Mode().Perm() != tc.want {
			t.Errorf("%s mode = %04o, want %04o", tc.path, st.Mode().Perm(), tc.want)
		}
	}
	// Tightening is scoped to what the mirror owns; the user's own file keeps
	// the mode it had.
	if st, err := os.Stat(own); err != nil {
		t.Fatalf("stat user file: %v", err)
	} else if st.Mode().Perm() != 0o644 {
		t.Errorf("user file mode = %04o, want it left at 0644", st.Mode().Perm())
	}
}
