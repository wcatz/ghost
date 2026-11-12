//go:build windows

package memory

import "os"

// keyFileOpenFlags opens the retrieval key read-only.
const keyFileOpenFlags = os.O_RDONLY
