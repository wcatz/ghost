//go:build !windows

package claudeimport

import (
	"os"
	"syscall"
)

const memoryFileOpenFlags = os.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
