//go:build windows

package memory

// TightenPermissions is a no-op on Windows. Access there is carried by an ACL
// inherited from the parent directory, not by the POSIX mode bits that
// os.Chmod maps onto read-only; a 0600 request would not stop another account
// with access to the directory, and a mode-tightening pass would only report
// numbers that do not describe the access actually granted. The directory ACL
// is the thing to tighten on this platform, and nothing in Ghost sets it
// today — see the Windows note in docs/configuration.md.
func TightenPermissions(dbPath string) {}
