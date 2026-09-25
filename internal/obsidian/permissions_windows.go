//go:build windows

package obsidian

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

// protectPath restricts a path to its owner by writing a protected DACL.
// Windows has no POSIX mode: os.Chmod there only toggles the read-only bit
// and never removes inherited ACL access, so the numeric 0700/0600 this
// package has always passed left the vault readable by every account on
// the machine. The DACL is written on every tighten pass rather than
// compared: deciding "already tight" costs a security-descriptor read per
// file, i.e. one syscall more than writing one.
func protectPath(path string, _ os.FileInfo, _ os.FileMode) error {
	return setPrivateDACL(path)
}

// protectFile secures the temp file before it is renamed into place. Set by
// name: the temp name is unique inside the vault directory and has not been
// published yet, and Windows has no fchmod equivalent.
func protectFile(f *os.File) error {
	return setPrivateDACL(f.Name())
}

// setPrivateDACL replaces the DACL of path with one granting full control
// to the current user, SYSTEM, and Administrators — and nobody else — and
// marks it protected so nothing is inherited back from the parent. SYSTEM
// and Administrators are kept for backup and servicing; the owner alone
// would lock administrators out of a vault they must be able to recover.
func setPrivateDACL(path string) error {
	acl, err := privateACL()
	if err != nil {
		return err
	}
	abs, err := extendedPath(path)
	if err != nil {
		return err
	}
	if err := windows.SetNamedSecurityInfo(
		abs,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil,
	); err != nil {
		return fmt.Errorf("set vault DACL on %s: %w", path, err)
	}
	return nil
}

// privateACL builds the owner/SYSTEM/Administrators full-control DACL. The
// ACEs are inheritable so files Ghost writes later inside a protected
// directory inherit the same restriction.
func privateACL() (*windows.ACL, error) {
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("current user SID: %w", err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return nil, fmt.Errorf("SYSTEM SID: %w", err)
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return nil, fmt.Errorf("Administrators SID: %w", err)
	}
	allow := func(sid *windows.SID, trusteeType uint32) windows.EXPLICIT_ACCESS {
		return windows.EXPLICIT_ACCESS{
			AccessPermissions: windows.ACCESS_MASK(windows.GENERIC_ALL),
			AccessMode:        windows.SET_ACCESS,
			Inheritance:       windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_TYPE(trusteeType),
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		}
	}
	return windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{
		allow(tokenUser.User.Sid, windows.TRUSTEE_IS_USER),
		allow(system, windows.TRUSTEE_IS_WELL_KNOWN_GROUP),
		allow(admins, windows.TRUSTEE_IS_WELL_KNOWN_GROUP),
	}, nil)
}

// extendedPath returns path in \\?\ form. SetNamedSecurityInfo is a raw
// Win32 path API that does not get Go's long-path handling, so a vault
// deeper than MAX_PATH would fail to secure without it.
func extendedPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(abs, `\\`) {
		return `\\?\UNC\` + abs[2:], nil
	}
	return `\\?\` + abs, nil
}

// isDirNotEmpty reports whether os.Remove failed only because the directory
// still holds entries — the guard doing its job — as opposed to a
// permission, sharing, or I/O failure that prune must surface. On Windows
// that is ERROR_DIR_NOT_EMPTY; Go's syscall.ENOTEMPTY is an unrelated
// application-error sentinel here.
func isDirNotEmpty(err error) bool {
	return errors.Is(err, syscall.ERROR_DIR_NOT_EMPTY)
}

// makeFIFO reports that the special-file prune regression cannot run here:
// Windows has no filesystem FIFOs (and the walk never opens one, since the
// guard is on the file type).
func makeFIFO(path string) error {
	return errors.New("named pipes are not a filesystem concept on Windows")
}
