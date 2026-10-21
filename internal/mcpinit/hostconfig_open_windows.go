//go:build windows

package mcpinit

import "os"

// hostConfigOpenFlags are the flags readHostConfigFile opens a host
// configuration file with.
//
// There is no O_NONBLOCK on Windows in Go's syscall package, and a FIFO in the
// unix sense does not exist there, so the fstat on the open descriptor is the
// whole of the regular-file check.
const hostConfigOpenFlags = os.O_RDONLY
