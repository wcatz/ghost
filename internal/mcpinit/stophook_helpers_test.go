package mcpinit

// Shared stop-hook test fixtures: the spawn recorder and the prepared project
// sandbox. They live here rather than in any one test file because several
// unrelated tests need the same isolated home, config and project.

import (
	"os/exec"
	"testing"
)

// captureLifecycleChild replaces the spawn with a recorder for the duration of
// one test and returns the argv it was handed.
func captureLifecycleChild(t *testing.T) *[][]string {
	t.Helper()
	real := startLifecycleChild
	t.Cleanup(func() { startLifecycleChild = real })

	var seen [][]string
	startLifecycleChild = func(cmd *exec.Cmd) error {
		seen = append(seen, cmd.Args)
		return nil
	}
	return &seen
}
