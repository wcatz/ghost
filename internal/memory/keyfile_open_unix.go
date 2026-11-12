//go:build !windows

package memory

import (
	"os"
	"syscall"
)

// keyFileOpenFlags are the flags the retrieval key is opened with: O_NOFOLLOW
// makes the kernel refuse a symlink planted at the path, and O_NONBLOCK keeps a
// FIFO from blocking the open until a writer appears. The fstat in readKeyFile
// then refuses anything that is not a regular file. O_NONBLOCK changes nothing
// for a regular file.
const keyFileOpenFlags = os.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
