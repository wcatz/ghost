//go:build !windows

package mcpinit

import (
	"os"
	"syscall"
)

const markerOpenFlags = os.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
const momentAppendOpenFlags = os.O_APPEND | os.O_CREATE | os.O_WRONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
const logAppendOpenFlags = os.O_APPEND | os.O_CREATE | os.O_WRONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
const operatorLogOpenFlags = os.O_APPEND | os.O_CREATE | os.O_WRONLY | syscall.O_NONBLOCK
