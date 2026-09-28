//go:build !windows

package memory

import (
	"os"
	"syscall"
)

// manifestOpenFlags are the flags a manifest sidecar is created with, and the
// reason they are named here rather than written at the call site.
//
// O_NOFOLLOW is the enforcement; the Lstat that precedes the open is only the
// classification. reserveBackupPath gets the same guarantee for free from
// O_EXCL — its own comment says so, that O_EXCL is the atomic claim and there is
// no gap between the Lstat and the create — but a manifest is REPLACED rather
// than refused (it is derived from the snapshot beside it, so one found at that
// path belongs to an already-deleted backup), and O_EXCL would refuse exactly
// the case the replace exists to serve. Without a flag in its place the Lstat is
// a stat-then-create pair: a symlink planted in the window between them is
// followed, and the target is truncated, overwritten with a manifest and
// narrowed to 0600 — a file the user never named, in a --out directory they
// chose. O_NOFOLLOW makes the kernel refuse that open instead, so the window
// closes even though the path is not exclusively claimed.
const manifestOpenFlags = os.O_CREATE | os.O_WRONLY | syscall.O_NOFOLLOW
