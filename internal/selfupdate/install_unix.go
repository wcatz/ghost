//go:build !windows

package selfupdate

// asideRunningTarget is the install policy on this platform. Unix renames
// atomically over the target, so the binary being replaced needs no move out
// of the way first. See installNewBinary.
const asideRunningTarget = false
