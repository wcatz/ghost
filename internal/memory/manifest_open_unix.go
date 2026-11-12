//go:build !windows

package memory

import (
	"os"
	"syscall"
)

// manifestOpenFlags creates or opens a manifest sidecar for writing without following symlinks.
const manifestOpenFlags = os.O_CREATE | os.O_WRONLY | syscall.O_NOFOLLOW
