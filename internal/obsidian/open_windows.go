//go:build windows

package obsidian

import "os"

// obsidianOpenFlags are the flags writeIfChanged opens a note path with when
// the path is known to be a regular file (Lstat confirmed it). Windows has
// no O_NOFOLLOW or O_NONBLOCK equivalent in Go's syscall package, so the
// Lstat before the open and the fstat after it are the whole of the defence;
// a FIFO in the unix sense does not exist there. The flags only guard
// against a swap between Lstat and open; they do not control symlink
// handling for symlinks already present at Lstat time (those are handled
// separately with obsidianFollowOpenFlags).
const obsidianOpenFlags = os.O_RDONLY

// obsidianFollowOpenFlags are the flags hasGhostID and hasGhostContent open
// a note path with. On Windows the Lstat and fstat are the whole defence.
const obsidianFollowOpenFlags = os.O_RDONLY
