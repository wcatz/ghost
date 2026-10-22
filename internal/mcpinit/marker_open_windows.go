//go:build windows

package mcpinit

import "os"

// markerOpenFlags are the flags openMarker opens a failure marker with.
//
// There is no O_NOFOLLOW or O_NONBLOCK on Windows in Go's syscall package, so
// the fstat after the open is the whole of the defence here: a reparse point
// swapped in before the open would be followed, and the fstat still refuses a
// target that is not a regular file. Creating a link on Windows needs a
// privilege or developer mode, and a FIFO in the unix sense does not exist
// there.
const markerOpenFlags = os.O_RDONLY

// momentAppendOpenFlags are the flags appendMomentEntry opens the working-moment
// marker with. Windows has no O_NOFOLLOW or O_NONBLOCK in Go's syscall package,
// so the fstat after the open is the whole of the defence: a reparse point
// swapped in before the open would be followed, and the fstat still refuses a
// target that is not a regular file.
const momentAppendOpenFlags = os.O_APPEND | os.O_CREATE | os.O_WRONLY
