//go:build !windows

package audit

import (
	"os"
	"syscall"
)

// sidecarOpenFlags are the flags the sweep opens a sidecar with. The lstat
// from the directory read is the classification, and these flags are the
// enforcement, the same split internal/claudeimport's memoryFileOpenFlags
// makes: O_NOFOLLOW makes the kernel refuse a symlink swapped in between the
// lstat and the open, and O_NONBLOCK keeps a FIFO swapped in during that
// window from blocking the open until a writer appears. Such a FIFO is then
// refused by the fstat on the open descriptor. O_NONBLOCK changes nothing
// for a regular file.
const sidecarOpenFlags = os.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
