//go:build windows

package obsidian

import "os"

const obsidianOpenFlags = os.O_RDONLY

const obsidianFollowOpenFlags = os.O_RDONLY
