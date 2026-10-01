package audit

import (
	"os"
	"time"
)

// The three helpers below are file plumbing shared by the sidecar tests: two
// write/read a path and one makes a file look old enough to sweep. They are here
// rather than inline because each is used by more than one test, and a test that
// copied a `os.Chtimes` call into its own body would be asserting against a file
// it had aged with a different spelling of the same three lines.

func writeFileString(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

func readFileString(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// touchOlderThan moves a file's modification time into the past, which is how the
// sweep test gets a file that LOOKS stale without waiting a day for one to
// become so.
func touchOlderThan(path string, age time.Duration) error {
	when := time.Now().Add(-age)
	return os.Chtimes(path, when, when)
}
