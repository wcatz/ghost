//go:build unix

package mcpinit

import (
	"io/fs"
	"os"
	"syscall"
)

func ownedByCurrentUser(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int64(st.Uid) == int64(os.Getuid())
}
