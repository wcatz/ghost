//go:build unix

package mcpinit

import (
	"io/fs"
	"os"
	"syscall"
)

// ownedByCurrentUser reports whether info names an entry owned by this
// process's user. An entry whose owner cannot be read is not ours to remove.
func ownedByCurrentUser(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int64(st.Uid) == int64(os.Getuid())
}
