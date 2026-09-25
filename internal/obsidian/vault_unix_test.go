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

// TestEnsureVaultLeavesUserPermissionsAlone is the architect's ruling, on
// disk. An earlier revision walked the whole vault on every ensureVault and
// chmod'd every directory to 0700 — including the user's own folders and
// .obsidian/, which Ghost never created — and on Windows rewrote a protected
// DACL on each of them on every pass. A mirror reads those paths; it does not
// own them, and changing their permissions is not its business.
//
// Ghost still sets 0700/0600 on everything it creates and writes, in
// writeIfChanged, via protectFile. This asserts only the other half: a user's
// folder keeps the mode they gave it.
func TestEnsureVaultLeavesUserPermissionsAlone(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault")
	if err := ensureVault(root); err != nil {
		t.Fatalf("ensureVault: %v", err)
	}
	// The user's own directories, including the ones Obsidian itself makes.
	userDirs := []string{
		filepath.Join(root, "my notes"),
		filepath.Join(root, ".obsidian"),
		filepath.Join(root, ".obsidian", "workspace"),
	}
	for _, d := range userDirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatalf("chmod %s: %v", d, err)
		}
	}
	userFile := filepath.Join(root, "my notes", "shopping list.md")
	if err := os.WriteFile(userFile, []byte("# mine\n"), 0o644); err != nil {
		t.Fatalf("write user file: %v", err)
	}
	// WriteFile applies the umask, so the mode is set explicitly.
	if err := os.Chmod(userFile, 0o644); err != nil {
		t.Fatalf("chmod user file: %v", err)
	}

	if err := ensureVault(root); err != nil {
		t.Fatalf("ensureVault on a vault with user content: %v", err)
	}

	for _, d := range userDirs {
		st, err := os.Stat(d)
		if err != nil {
			t.Fatalf("stat %s: %v", d, err)
		}
		if st.Mode().Perm() != 0o755 {
			t.Errorf("%s mode = %04o, want 0755 — a mirror must not change the permissions of a folder it did not create", d, st.Mode().Perm())
		}
	}
	if st, err := os.Stat(userFile); err != nil {
		t.Fatalf("stat user file: %v", err)
	} else if st.Mode().Perm() != 0o644 {
		t.Errorf("user file mode = %04o, want 0644", st.Mode().Perm())
	}

	// The other half of the contract: what Ghost writes is still tight.
	note := filepath.Join(root, "proj", "Memories", "n.md")
	if _, err := writeIfChanged(note, ghostNote); err != nil {
		t.Fatalf("writeIfChanged: %v", err)
	}
	if st, err := os.Stat(note); err != nil {
		t.Fatalf("stat written note: %v", err)
	} else if st.Mode().Perm() != 0o600 {
		t.Errorf("written note mode = %04o, want 0600", st.Mode().Perm())
	}
}
