//go:build !windows

package mcpinit

import (
	"os"
	"syscall"
)

const hostConfigOpenFlags = os.O_RDONLY | syscall.O_NONBLOCK
