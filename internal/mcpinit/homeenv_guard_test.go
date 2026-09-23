package mcpinit

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

// TestHomeEnvIsolationGuard makes the home-directory isolation invariant
// self-enforcing: production resolves home via os.UserHomeDir, which reads
// HOME on Unix but USERPROFILE on Windows, so a sibling test that sets only
// one of them directly reads the runner's real profile on the other OS —
// exactly what the whole-package Windows legs must never do. Every
// *_test.go in this package must go through setHome (homeenv_test.go) for
// both variables, and this test fails the file:line of any direct
// t.Setenv("HOME"/"USERPROFILE") reintroduction.
func TestHomeEnvIsolationGuard(t *testing.T) {
	const helperFile = "homeenv_test.go" // where homeVars/setHome live
	const guardFile = "homeenv_guard_test.go"
	needles := []string{`t.Setenv("HOME"`, `t.Setenv("USERPROFILE"`}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		if name == helperFile || name == guardFile {
			// The helper is where setHome applies t.Setenv itself; this
			// file quotes the forbidden call shapes as needle literals.
			continue
		}
		scanForBareHomeSetenv(t, name, needles)
	}
}

// scanForBareHomeSetenv fails with file:line for every non-comment line of
// path that sets HOME or USERPROFILE directly instead of via setHome.
func scanForBareHomeSetenv(t *testing.T, path string, needles []string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		trimmed := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
			continue
		}
		for _, needle := range needles {
			if strings.Contains(trimmed, needle) {
				t.Errorf("%s:%d sets HOME/USERPROFILE directly; use setHome(t, dir) so both variables stay in sync", path, line)
				break
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
}
