//go:build windows

package mcpinit

import "testing"

// TestIsHomeOrRootRecognisesWindowsRoots pins the #1299 defect: a Windows
// session at a drive root or a network share root was never recognised as a
// root, because the check compared the cwd against the literal "/" that no
// Windows path can equal. Only a Windows build can produce these paths.
func TestIsHomeOrRootRecognisesWindowsRoots(t *testing.T) {
	for _, cwd := range []string{
		`C:\`,
		`D:\`,
		`C:/`,
		`\\server\share\`,
	} {
		if !isHomeOrRoot(cwd) {
			t.Errorf("isHomeOrRoot(%q) = false, want true", cwd)
		}
	}
	for _, cwd := range []string{
		`C:\Users\me`,
		`\\server\share\project`,
	} {
		if isHomeOrRoot(cwd) {
			t.Errorf("isHomeOrRoot(%q) = true, want false", cwd)
		}
	}
}
