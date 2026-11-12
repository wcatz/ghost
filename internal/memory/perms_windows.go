//go:build windows

package memory

// TightenPermissions is a no-op on Windows.
func TightenPermissions(dbPath string) {}
