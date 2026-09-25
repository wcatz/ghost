//go:build windows

package obsidian

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// TestVaultIsPrivateOnWindows: Windows has no POSIX mode, and os.Chmod
// there only toggles the read-only bit — the 0700/0600 this package has
// always passed never removed inherited ACL access, so the vault was
// readable by every account on the machine. The guarantee is a protected
// DACL naming only the owner, SYSTEM, and Administrators.
func TestVaultIsPrivateOnWindows(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault")
	if err := ensureVault(root); err != nil {
		t.Fatalf("ensureVault: %v", err)
	}
	assertPrivate(t, root)
	assertPrivate(t, filepath.Join(root, markerName))

	// A written note exercises both the nested directory and the file itself.
	note := filepath.Join(root, "proj", "Notes", "n.md")
	if _, err := writeIfChanged(note, "hello\n"); err != nil {
		t.Fatalf("writeIfChanged: %v", err)
	}
	assertPrivate(t, filepath.Dir(note))
	assertPrivate(t, note)
}

// TestEnsureVaultTightensLegacyPermissionsOnWindows: an adopted vault
// keeps the DACL it was created with unless ensureVault rewrites it — the
// Windows half of the "legacy vaults stay readable" defect. The protected
// flag is the load-bearing assertion: a default temp-dir file can already
// happen to list only these three SIDs by inheritance, and only a
// protected, explicitly written DACL proves Ghost did the work.
func TestEnsureVaultTightensLegacyPermissionsOnWindows(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault")
	proj := filepath.Join(root, "proj")
	memories := filepath.Join(proj, "Memories")
	mustMkdirAll(t, memories)
	mustWrite(t, filepath.Join(root, markerName), `{"schema_version":1}`+"\n")
	mustWrite(t, filepath.Join(memories, "n.md"), ghostNote)
	// The user's own file keeps whatever access its parent inheritance gave it.
	own := filepath.Join(proj, "own.md")
	mustWrite(t, own, userNote)

	if err := ensureVault(root); err != nil {
		t.Fatalf("ensureVault: %v", err)
	}
	for _, p := range []string{root, proj, memories, filepath.Join(root, markerName), filepath.Join(memories, "n.md")} {
		assertPrivate(t, p)
	}
	if _, err := os.Stat(own); err != nil {
		t.Fatalf("the user's file must still exist: %v", err)
	}
}

// assertPrivate fails unless path carries a protected DACL that grants
// access to the current user, SYSTEM, and Administrators only.
func assertPrivate(t *testing.T, path string) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("security descriptor of %s: %v", path, err)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatalf("descriptor control of %s: %v", path, err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Errorf("%s DACL is still inherited from its parent — nobody wrote an explicit access list", path)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatalf("DACL of %s: %v", path, err)
	}
	if dacl == nil {
		t.Fatalf("%s has a NULL DACL — every account has full control", path)
	}
	if dacl.AceCount == 0 {
		t.Fatalf("%s has an empty DACL — nobody, not even the owner, has access", path)
	}
	allowed := allowedSIDs(t)
	for i := uint16(0); i < dacl.AceCount; i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(i), &ace); err != nil {
			t.Fatalf("ACE %d of %s: %v", i, path, err)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
		if !allowed[sid] {
			t.Errorf("%s grants %s access; only the owner, SYSTEM (S-1-5-18), and Administrators (S-1-5-32-544) may", path, sid)
		}
	}
}

// allowedSIDs are the three trustees setPrivateDACL names.
func allowedSIDs(t *testing.T) map[string]bool {
	t.Helper()
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatalf("current user SID: %v", err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatalf("SYSTEM SID: %v", err)
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatalf("Administrators SID: %v", err)
	}
	return map[string]bool{
		tokenUser.User.Sid.String(): true,
		system.String():             true,
		admins.String():             true,
	}
}
