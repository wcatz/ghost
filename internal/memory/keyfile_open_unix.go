//go:build !windows

package memory

import (
	"os"
	"syscall"
)

// keyFileOpenFlags opens the retrieval key read-only and refuses symlinks and blocking FIFOs.
const keyFileOpenFlags = os.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
