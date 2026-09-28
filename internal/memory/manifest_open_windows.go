//go:build windows

package memory

import "os"

// manifestOpenFlags are the flags a manifest sidecar is created with.
//
// There is no O_NOFOLLOW on Windows — Go's syscall package does not define one —
// so the Lstat in writeBackupManifest is the whole of the defence here, and the
// window it leaves is not closable with the flags this platform has. It is
// stated rather than papered over: a reparse point planted between the
// classification and the open would be followed, and the same target-narrowing
// damage would follow it. The exposure is much smaller than on unix, because
// creating a symlink on Windows needs a privilege or developer mode that unix
// does not, and because a manifest path inside a user-chosen --out directory is
// not a place a link is planted by accident.
//
// TightenPermissions is a no-op on Windows for the same reason the mode is not
// enforced there at all: access is carried by an ACL, not by a mode bit.
const manifestOpenFlags = os.O_CREATE | os.O_WRONLY
