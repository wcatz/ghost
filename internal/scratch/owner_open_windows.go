//go:build windows

package scratch

import "os"

// ownerOpenFlags are the flags ownerMarker opens a marker with. There is no
// O_NOFOLLOW or O_NONBLOCK on Windows in Go's syscall package, so the lstat
// before the open and the fstat after it are the whole of the defence.
const ownerOpenFlags = os.O_RDONLY
