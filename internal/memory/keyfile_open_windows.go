//go:build windows

package memory

import "os"

// keyFileOpenFlags are the flags the retrieval key is opened with. Windows has no
// O_NOFOLLOW or O_NONBLOCK in Go's syscall package, so the fstat in readKeyFile
// is the whole of the defence: a target that is not a regular file is refused.
const keyFileOpenFlags = os.O_RDONLY
