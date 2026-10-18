//go:build windows

package claudeimport

import "os"

// memoryFileOpenFlags are the flags ParseMemoryFile opens a memory file with.
//
// There is no O_NOFOLLOW or O_NONBLOCK on Windows in Go's syscall package, so
// the Lstat before the open and the fstat after it are the whole of the
// defence here: a reparse point planted between the two would be followed,
// and the fstat still refuses a target that is not a regular file. Creating a
// link on Windows needs a privilege or developer mode, and a FIFO in the unix
// sense does not exist there.
const memoryFileOpenFlags = os.O_RDONLY
