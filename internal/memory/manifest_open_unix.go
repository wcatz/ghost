//go:build !windows

package memory

import (
	"os"
	"syscall"
)

// manifestOpenFlags are the flags a manifest sidecar is created with, and the
// reason they are named here rather than written at the call site.
const manifestOpenFlags = os.O_CREATE | os.O_WRONLY | syscall.O_NOFOLLOW
