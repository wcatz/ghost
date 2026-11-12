//go:build !windows

package obsidian

import (
	"os"
	"syscall"
)

const obsidianOpenFlags = os.O_RDONLY | syscall.O_NONBLOCK | syscall.O_NOFOLLOW

const obsidianFollowOpenFlags = os.O_RDONLY | syscall.O_NONBLOCK
