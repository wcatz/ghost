//go:build !windows

package mcpinit

import (
	"os"
	"syscall"
)

// markerOpenFlags are the flags openMarker opens a failure marker with. The
// same split the sidecar reads make: O_NOFOLLOW makes the kernel refuse a
// symlink planted before the open, and O_NONBLOCK keeps a FIFO from blocking
// the open until a writer appears; such a FIFO is then refused by the fstat
// on the open descriptor. O_NONBLOCK changes nothing for a regular file.
const markerOpenFlags = os.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
