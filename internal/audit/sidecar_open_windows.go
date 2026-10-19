//go:build windows

package audit

import "os"

// sidecarOpenFlags are the flags the sweep opens a sidecar with.
//
// There is no O_NOFOLLOW or O_NONBLOCK on Windows in Go's syscall package, so
// the lstat before the open and the fstat after it are the whole of the
// defence here: a reparse point swapped in between the two would be followed,
// and the fstat still refuses a target that is not a regular file. Creating a
// link on Windows needs a privilege or developer mode, and a FIFO in the unix
// sense does not exist there.
const sidecarOpenFlags = os.O_RDONLY
