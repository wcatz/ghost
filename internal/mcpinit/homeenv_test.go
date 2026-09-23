package mcpinit

import "testing"

// homeVars returns the HOME/USERPROFILE assignments that isolate a home
// directory on every platform os.UserHomeDir reads: HOME on Unix,
// USERPROFILE on Windows. Production resolves home via os.UserHomeDir, so a
// test that sets only HOME reads the real profile on Windows.
func homeVars(dir string) [][2]string {
	return [][2]string{{"HOME", dir}, {"USERPROFILE", dir}}
}

// setHome applies homeVars with t.Setenv (restored after the test).
func setHome(t *testing.T, dir string) {
	t.Helper()
	for _, kv := range homeVars(dir) {
		t.Setenv(kv[0], kv[1])
	}
}
