package main

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// contextSessionCount reads the session counter a live `ghost context` render
// bumps for project, straight from the store contextAsOfStore built. A missing
// row is a count of 0: the counter row is created by the first bump.
func contextSessionCount(t *testing.T, project string) int {
	t.Helper()
	db, err := memory.OpenDB(filepath.Join(os.Getenv("XDG_DATA_HOME"), "ghost", "ghost.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close() //nolint:errcheck
	var n int
	err = db.QueryRow(`SELECT interaction_count FROM ghost_state WHERE project_id = ?`, project).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0
	}
	if err != nil {
		t.Fatalf("read interaction_count: %v", err)
	}
	return n
}

// contextDataDirEntries lists the data directory, so a refusal that wrote a
// working-moment ledger, a marker or any other file beside the store shows up as
// a new name.
func contextDataDirEntries(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(os.Getenv("XDG_DATA_HOME"), "ghost"))
	if err != nil {
		t.Fatalf("read data dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// runContextCLI drives `ghost <argv...>` through the real dispatch and returns
// the exit code and both streams.
func runContextCLI(t *testing.T, argv ...string) (code int, stdout, stderr string) {
	t.Helper()
	origArgs := os.Args
	os.Args = append([]string{origArgs[0]}, argv...)
	defer func() { os.Args = origArgs }()
	stdout, stderr = captureStreams(t, func() {
		code = runCLI(os.Args[1:], dispatchCommand)
	})
	return code, stdout, stderr
}

// TestContextPlainRefusalRunsNoSideEffect: `ghost context` without --audit is a
// session start — it bumps the project's session count (and starts the Obsidian
// mirror, which no test can see: it is skipped under `go test`). An argument the
// block does not take used to be dropped and the live block rendered anyway, so
// `ghost context --project other` counted a session for the directory's project
// and printed a block for a scope nobody asked about (#1195). Each of these must
// be refused with exit 1 BEFORE the render: nothing on stdout, the count
// untouched, nothing new in the data directory.
func TestContextPlainRefusalRunsNoSideEffect(t *testing.T) {
	dir := contextAsOfStore(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if code, stdout, stderr := runContextCLI(t, "context", "--cwd", dir); code != 0 || !strings.Contains(stdout, "the nightly sweep compacts the WAL file") {
		t.Fatalf("control render: code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	before := contextSessionCount(t, "cliproj")
	if before != 1 {
		t.Fatalf("control render left session count %d, want 1: the probe cannot see the side effect it guards", before)
	}
	filesBefore := strings.Join(contextDataDirEntries(t), ",")

	for _, argv := range [][]string{
		{"context", "--cwd", dir, "--bogus"},
		{"context", "--cwd", dir, "stray"},
		{"context", "--cwd", dir, "--project", "cliproj"},
		{"context", "--cwd", dir, "--project=cliproj"},
		{"context", "--cwd", dir, "--since", "1h"},
		{"context", "--cwd", dir, "--since=1h"},
		{"context", "--cwd", dir, "--cwd"},
		{"context", "--cwd", dir, "--cwd", dir},
	} {
		t.Run(strings.Join(argv[3:], " "), func(t *testing.T) {
			before := contextSessionCount(t, "cliproj")
			code, stdout, stderr := runContextCLI(t, argv...)
			if code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want nothing: a refused request renders no block", stdout)
			}
			if !strings.HasPrefix(stderr, "ghost context: ") {
				t.Errorf("stderr = %q, want a diagnostic naming the command", stderr)
			}
			if got := contextSessionCount(t, "cliproj"); got != before {
				t.Errorf("session count moved from %d to %d: the refusal ran the session-start side effects", before, got)
			}
			if got := strings.Join(contextDataDirEntries(t), ","); got != filesBefore {
				t.Errorf("data directory changed from [%s] to [%s]", filesBefore, got)
			}
		})
	}
}
