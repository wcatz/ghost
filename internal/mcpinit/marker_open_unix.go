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

// momentAppendOpenFlags are the flags appendMomentEntry opens the working-moment
// marker with. They are the write side of the same guard markerOpenFlags gives
// the read side: O_NOFOLLOW makes the kernel refuse a symlink planted at the
// path, O_NONBLOCK keeps a FIFO from blocking the open until a reader appears,
// and the fstat on the open descriptor refuses anything that is not a regular
// file before a byte is written. O_NONBLOCK changes nothing for a regular file.
const momentAppendOpenFlags = os.O_APPEND | os.O_CREATE | os.O_WRONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK

// logAppendOpenFlags are the flags createLog opens a data-dir log with. The
// same guard as momentAppendOpenFlags: O_NOFOLLOW makes the kernel refuse a
// symlink planted at the log's name, O_NONBLOCK keeps a FIFO from blocking the
// open until a reader appears, and the fstat on the open descriptor refuses
// anything that is not a regular file before a byte is written.
const logAppendOpenFlags = os.O_APPEND | os.O_CREATE | os.O_WRONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK

// operatorLogOpenFlags are logAppendOpenFlags without O_NOFOLLOW, for the log
// path the operator chose: a symlink is followed, and O_NONBLOCK plus the fstat
// in openRegularAppend still refuse a FIFO or other special file.
const operatorLogOpenFlags = os.O_APPEND | os.O_CREATE | os.O_WRONLY | syscall.O_NONBLOCK
