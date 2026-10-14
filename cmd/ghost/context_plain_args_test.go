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
//
// The control run first proves the probe can see the write it is guarding
// against: a plain render of the same directory does move the count.
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

// TestContextPlainRefusalNamesTheAuditFlags: --project and --since are real flags
// of `ghost context`, but of its --audit mode. The refusal says so, because the
// reader who typed them most likely wanted the report.
func TestContextPlainRefusalNamesTheAuditFlags(t *testing.T) {
	for _, argv := range [][]string{
		{"--project", "ghost"},
		{"--project=ghost"},
		{"--since", "1h"},
		{"--since=1h"},
	} {
		_, _, err := parseContextBlockArgs(argv)
		if err == nil || !strings.Contains(err.Error(), "--audit") {
			t.Errorf("parseContextBlockArgs(%v) error = %v, want a refusal naming --audit", argv, err)
		}
	}
}

// TestContextPlainArgsAccepted: the flags the block does take still parse, and
// an EMPTY --cwd value stays what it has always been — the process's own
// directory — because the opencode adapter spawns `ghost context --cwd <dir>`
// with whatever directory it was handed, and a refusal there would be a session
// with no context rather than a visible error.
func TestContextPlainArgsAccepted(t *testing.T) {
	for _, tc := range []struct {
		argv    []string
		wantCwd string
		wantAt  bool
	}{
		{nil, "", false},
		{[]string{"--cwd", "/x"}, "/x", false},
		{[]string{"--cwd=/x"}, "/x", false},
		{[]string{"--cwd", ""}, "", false},
		{[]string{"--cwd="}, "", false},
		{[]string{"--cwd", "/x", "--as-of", "2026-01-01T00:00:00Z"}, "/x", true},
		{[]string{"--as-of=2026-01-01T00:00:00Z", "--cwd=/x"}, "/x", true},
		// A dash-leading directory is a value, not a flag: the token after --cwd
		// is taken whatever it looks like, as --project's is under --audit.
		{[]string{"--cwd", "-odd"}, "-odd", false},
	} {
		cwd, at, err := parseContextBlockArgs(tc.argv)
		if err != nil {
			t.Errorf("parseContextBlockArgs(%q) = %v, want no error", tc.argv, err)
			continue
		}
		if cwd != tc.wantCwd || (at != nil) != tc.wantAt {
			t.Errorf("parseContextBlockArgs(%q) = (%q, %v), want (%q, as-of %v)", tc.argv, cwd, at, tc.wantCwd, tc.wantAt)
		}
	}
}
