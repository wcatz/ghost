//go:build windows

package config

import "os"

// configOpenFlags are the flags readConfigLayer opens a config layer with.
//
// There is no O_NONBLOCK on Windows in Go's syscall package, and a FIFO in the
// unix sense does not exist there, so the fstat on the open descriptor is the
// whole of the regular-file check.
const configOpenFlags = os.O_RDONLY
