package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// contextAsOfStore builds a store in an isolated data home with one project
// matching dir and one memory in it, inserted with SQL so it has no recorded
// version of itself — the state every pre-v17 memory is in, and the one a
// historical read has to disclose rather than fill with today's text.
func contextAsOfStore(t *testing.T) (dir string) {
	t.Helper()
	xdgHome := t.TempDir()
	ghostDir := filepath.Join(xdgHome, "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir ghostDir: %v", err)
	}
	dir = t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}

	db, err := memory.OpenDB(filepath.Join(ghostDir, "ghost.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES (?, ?, ?)`, "cliproj", dir, "cliproj"); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO memories (id, project_id, category, content, source) VALUES (?, ?, ?, ?, ?)`,
		"climem1", "cliproj", "fact", "the nightly sweep compacts the WAL file", "manual",
	); err != nil {
		t.Fatalf("insert memory: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	t.Setenv("XDG_DATA_HOME", xdgHome)
	t.Setenv("HOME", t.TempDir())
	return dir
}

// TestContextAsOfFlagReachesTheRenderer: the flag is the CLI half of #647, and
// the thing that can go wrong at a boundary is the value never arriving. Both
// spellings are checked because the hand-rolled parser accepts them separately,
// and one of them being wired to nothing would leave the flag looking supported
// while printing the present.
func TestContextAsOfFlagReachesTheRenderer(t *testing.T) {
	dir := contextAsOfStore(t)
	for _, argv := range [][]string{
		{"--cwd", dir, "--as-of", "2035-01-01T00:00:00Z"},
		{"--cwd", dir, "--as-of=2035-01-01T00:00:00Z"},
	} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			origArgs := os.Args
			// argv[0] stays the program name and argv[1] is the subcommand, which
			// is the shape runContext's hand-rolled scan indexes from.
			os.Args = append([]string{origArgs[0], "context"}, argv...)
			defer func() { os.Args = origArgs }()
			stdout, stderr := captureStreams(t, func() { runContext() })
			if !strings.Contains(stdout, "as_of 2035-01-01T00:00:00Z") {
				t.Errorf("the block does not name the instant the flag asked for:\n%s\nstderr: %s", stdout, stderr)
			}
			if !strings.Contains(stdout, "unknown before its first recorded version") {
				t.Errorf("the block does not report the memory it cannot place:\n%s", stdout)
			}
			if strings.Contains(stdout, "the nightly sweep compacts the WAL file") {
				t.Errorf("the block printed today's text for a memory with no recorded version:\n%s", stdout)
			}
		})
	}
}
