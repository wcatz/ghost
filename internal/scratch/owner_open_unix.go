//go:build !windows

package scratch

import (
	"os"
	"syscall"
)

const ownerOpenFlags = os.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
