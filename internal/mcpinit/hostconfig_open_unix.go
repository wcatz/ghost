//go:build !windows

package mcpinit

import (
	"os"
	"syscall"
)

// hostConfigOpenFlags are the flags readHostConfigFile opens a host
// configuration file with. O_NONBLOCK keeps a FIFO at the path from blocking
// the open until a writer appears; the fstat on the open descriptor then
// refuses it. It changes nothing for a regular file. There is deliberately no
// O_NOFOLLOW, unlike the marker readers: a symlinked settings.json or
// config.toml is a legitimate setup, and the regular-file check applies to
// the link's target.
const hostConfigOpenFlags = os.O_RDONLY | syscall.O_NONBLOCK
