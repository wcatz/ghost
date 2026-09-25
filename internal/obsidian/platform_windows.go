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
// permission, sharing, or I/O failure that prune must surface.
//
// On Windows that is NOT simply ERROR_DIR_NOT_EMPTY. Go's os.Remove tries
// DeleteFile first, which cannot remove a directory and fails with
// ERROR_ACCESS_DENIED, and only then RemoveDirectory. When the directory is
// not empty the two calls report different things, and Go returns the FIRST
// error unless the attribute probe says otherwise — so the caller sees
// ERROR_ACCESS_DENIED and never ERROR_DIR_NOT_EMPTY at all. With the old
// predicate this was therefore never true on Windows, and a single user file
// beside a Ghost note in an orphan folder failed the whole export instead of
// producing the ordinary "still holds something" answer.
//
// ERROR_ACCESS_DENIED is so treated as well. The cost is that an empty but
// locked directory is left in place too — the same outcome, retried on the next
// export, which is the recoverable direction. Failing the export is not.
func isDirNotEmpty(err error) bool {
	return errors.Is(err, syscall.ERROR_DIR_NOT_EMPTY) ||
		errors.Is(err, windows.ERROR_ACCESS_DENIED)
}

// makeFIFO reports that the special-file prune regression cannot run here:
// Windows has no filesystem FIFOs (and the walk never opens one, since the
// guard is on the file type).
func makeFIFO(path string) error {
	return errors.New("named pipes are not a filesystem concept on Windows")
}

// isTransientRenameErr reports whether a failed rename may succeed on a
// retry. MoveFileEx's replace step fails with a sharing violation whenever
// another process holds the destination open without FILE_SHARE_DELETE —
// Obsidian, Search Indexer, or Defender reading the vault is routine, not
// exceptional — and the same race between two concurrent syncs surfaces as
// ERROR_ACCESS_DENIED. A genuinely denied rename (read-only attribute, real
// DACL denial) just spends the same bounded backoff and then reports.
func isTransientRenameErr(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION) ||
		errors.Is(err, windows.ERROR_ACCESS_DENIED)
}

// renameNoReplace moves oldPath to newPath only if newPath does not exist.
// MoveFileEx without MOVEFILE_REPLACE_EXISTING is the no-replace rename here:
// it fails with ERROR_ALREADY_EXISTS rather than overwriting, and unlike the
// link(2) trick used on POSIX it needs no write permission on the directory
// beyond what the move itself requires.
func renameNoReplace(oldPath, newPath string) error {
	// Both operands go through extendedPath. MoveFileEx is a raw Win32 call
	// that gets none of Go's long-path handling, and the quarantine source sits
	// in the same possibly-over-MAX_PATH vault directory as the destination —
	// converting only the destination made a long-path restore fail, stranding
	// the note under its .ghost-prune-* name.
	from, err := extendedPath(oldPath)
	if err != nil {
		return err
	}
	fromPtr, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	to, err := extendedPath(newPath)
	if err != nil {
		return err
	}
	toPtr, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(fromPtr, toPtr, 0)
}

// openRegular opens path for reading and returns the handle only if the object
// it refers to is a regular file, decided by fstat on the handle being read.
// Windows has no filesystem FIFOs, so the blocking half of the POSIX problem
// does not exist here, but the file-type decision is still made on the opened
// object rather than on a directory entry stat'ed earlier, so a replacement
// between the two cannot be classified by the stale entry.
func openRegular(path string) (*os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, &os.PathError{Op: "open", Path: path, Err: windows.ERROR_ACCESS_DENIED}
	}
	return f, nil
}
