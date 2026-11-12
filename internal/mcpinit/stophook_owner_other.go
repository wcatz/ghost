//go:build !unix

package mcpinit

import "io/fs"

func ownedByCurrentUser(fs.FileInfo) bool { return true }
