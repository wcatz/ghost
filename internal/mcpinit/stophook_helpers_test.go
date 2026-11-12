package mcpinit

// Shared stop-hook test fixtures: the spawn recorder and the prepared project
// sandbox. They live here rather than in any one test file because several
// unrelated tests need the same isolated home, config and project.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// stopInputIn is a Stop payload naming the transcript at transcriptPath and the
// project directory cwd, in the host's format.
func stopInputIn(t *testing.T, cwd, transcriptPath, format string) string {
	t.Helper()
	return contractInputFor(t, "Stop", "claude-code", format,
		fmt.Sprintf(`{"session_id":"s1","transcript_path":%q,"cwd":%q,"stop_hook_active":false}`,
			transcriptPath, cwd))
}

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

// auditHookRun is one prepared stop-hook run: an isolated home with a store, a
// project the session's cwd resolves to, and a config that lets the lifecycle
// spawn. Returns the project directory to use as the payload's cwd.
func auditHookRun(t *testing.T) (dataHome, projDir string) {
	t.Helper()
	projDir, dataHome = isolatedProjectDir(t)
	// The install key. A live one always exists — the MCP server resolves it at
	// startup — and the retrieval record's query digest is keyed with it, so a
	// sandbox without one leaves the store's writes to the record refused.
	seedRetrievalKey(t, dataHome)
	return dataHome, projDir
}

// seedRetrievalKey writes an install key where a real install has one.
func seedRetrievalKey(t *testing.T, dataHome string) {
	t.Helper()
	dir := filepath.Join(dataHome, "ghost")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir the data directory: %v", err)
	}
	const keyHex = "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"
	path := filepath.Join(dir, "retrieval-record.key")
	if err := os.WriteFile(path, []byte(keyHex), 0o600); err != nil {
		t.Fatalf("write the install key: %v", err)
	}
}

// isolatedProjectDir is auditHookRun without its install key, for the one test
// whose subject IS the absent key. Everything else about the sandbox — the
// auto_resolve config that reaches the spawn, the project the cwd resolves to —
// has to match, or the chain would not run for a second and unrelated reason and
// the test would prove nothing.
func isolatedProjectDir(t *testing.T) (projDir, dataHome string) {
	t.Helper()
	dataHome = isolatedHome(t)
	writeGhostConfigFile(t, "reflection:\n  auto_resolve: true\n")
	t.Setenv("PATH", t.TempDir())

	projDir = filepath.Join(t.TempDir(), "audited")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	canonical, err := filepath.EvalSymlinks(projDir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	seedProject(t, dataHome, "p1", canonical, "audited")
	return canonical, dataHome
}
