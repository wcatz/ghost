//go:build !windows

package claudeimport

import (
	"os"
	"syscall"
)

// memoryFileOpenFlags are the flags ParseMemoryFile opens a memory file with.
// The Lstat that precedes the open is the classification, and these flags are
// the enforcement, the same split internal/memory's manifestOpenFlags makes:
// O_NOFOLLOW makes the kernel refuse a symlink planted between the Lstat and
// the open, and O_NONBLOCK keeps a FIFO planted in that window from blocking
// the open until a writer appears. Such a FIFO is then refused by the fstat
// on the open descriptor. O_NONBLOCK changes nothing for a regular file.
const memoryFileOpenFlags = os.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
