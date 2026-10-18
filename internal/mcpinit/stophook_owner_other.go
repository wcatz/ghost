//go:build !unix

package mcpinit

import "io/fs"

// ownedByCurrentUser has no owner to compare where the platform's file info
// does not carry a uid (Windows); the name, parent and layout checks in
// pluginSessionDir still apply there.
func ownedByCurrentUser(fs.FileInfo) bool { return true }
