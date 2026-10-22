//go:build !windows

package obsidian

import (
	"os"
	"syscall"
)

// obsidianOpenFlags are the flags writeIfChanged opens a note path with when
// the path is known to be a regular file (Lstat confirmed it). O_NONBLOCK
// keeps a FIFO swapped in after the Lstat from blocking the open, and
// O_NOFOLLOW refuses a symlink swapped in after the Lstat. The flags only
// guard against a swap between Lstat and open; they do not control symlink
// handling for symlinks already present at Lstat time (those are handled
// separately with obsidianFollowOpenFlags). O_NONBLOCK changes nothing for
// a regular file.
const obsidianOpenFlags = os.O_RDONLY | syscall.O_NONBLOCK | syscall.O_NOFOLLOW

// obsidianFollowOpenFlags are the flags hasGhostID and hasGhostContent open
// a note path with. O_NONBLOCK keeps a FIFO from blocking, but symlinks are
// followed so a symlinked Ghost note is still recognized and pruned. A
// non-regular target is refused on fstat.
const obsidianFollowOpenFlags = os.O_RDONLY | syscall.O_NONBLOCK
