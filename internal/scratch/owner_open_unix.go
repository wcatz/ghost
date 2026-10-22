//go:build !windows

package scratch

import (
	"os"
	"syscall"
)

// ownerOpenFlags are the flags ownerMarker opens a marker with. The entry's
// lstat is the classification and these flags are the enforcement, the same
// split internal/audit's sidecarOpenFlags makes: O_NOFOLLOW refuses a symlink
// swapped in between the lstat and the open, and O_NONBLOCK keeps a FIFO from
// blocking the open until a writer appears. Such a FIFO is then refused by the
// fstat on the open descriptor. O_NONBLOCK changes nothing for a regular file.
const ownerOpenFlags = os.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
