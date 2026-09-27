//go:build windows

package selfupdate

// asideRunningTarget is the install policy on this platform. A running
// executable cannot be renamed over, so the binary being replaced is moved
// aside first and the new one takes the path it vacates. See installNewBinary.
const asideRunningTarget = true
