package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	// Recorded against the RESOLVED path, because `ghost context` resolves the
	// directory it is given before matching it against a project's recorded path.
	// On Windows t.TempDir() returns a short-name path that EvalSymlinks expands,
	// so an unresolved fixture path matches no project and the block is empty.
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
			stdout, stderr := captureStreams(t, runContext)
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

// TestContextWithoutAsOfIsUnchanged: the flag is additive. The current render has
// to stay byte-identical, or a debugging tool has changed the session-start block
// every adapter injects.
func TestContextWithoutAsOfIsUnchanged(t *testing.T) {
	dir := contextAsOfStore(t)
	origArgs := os.Args
	os.Args = []string{origArgs[0], "context", "--cwd", dir}
	defer func() { os.Args = origArgs }()
	stdout, _ := captureStreams(t, runContext)
	if !strings.Contains(stdout, "the nightly sweep compacts the WAL file") {
		t.Errorf("the current block does not show the memory:\n%s", stdout)
	}
	if strings.Contains(stdout, "as_of ") {
		t.Errorf("a request with no --as-of produced a historical block:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Save new discoveries with ghost_memory_save") {
		t.Errorf("the current block lost its session instruction:\n%s", stdout)
	}
}

// TestContextRefusesAnInstantItCannotRead: an unreadable --as-of is a mistake at
// the boundary, and the command says so instead of printing the present. The parse
// is the thing under test rather than the process status, because the bad value
// ends in os.Exit and a test that drove that would take the package with it.
func TestContextRefusesAnInstantItCannotRead(t *testing.T) {
	for _, bad := range []string{"yesterday", "2020-01-01", "2020-01-01 00:00:00", "2035-01-01"} {
		got, err := contextAsOf([]string{"--as-of", bad})
		if err == nil {
			t.Errorf("--as-of %q parsed to %v, want a refusal", bad, got)
			continue
		}
		if got != nil {
			t.Errorf("--as-of %q returned %v alongside its error, want nil: a refused read has no instant", bad, got)
		}
		if !strings.Contains(err.Error(), "RFC 3339") {
			t.Errorf("--as-of %q was refused with %q, want the message to name the layout it could not read", bad, err)
		}
	}
}

// TestContextAsOfFlagIsOptional: an absent flag is a current read, not an error —
// which is what keeps every existing invocation of this command working.
func TestContextAsOfFlagIsOptional(t *testing.T) {
	for _, argv := range [][]string{
		nil,
		{"--cwd", "/tmp"},
		{"--cwd=/tmp"},
	} {
		got, err := contextAsOf(argv)
		if err != nil {
			t.Errorf("contextAsOf(%v) = %v, want no error: an absent flag is a current read", argv, err)
		}
		if got != nil {
			t.Errorf("contextAsOf(%v) = %v, want nil", argv, got)
		}
	}
}

// TestContextAsOfWithoutAValueIsRefused: `--as-of` with nothing usable after it
// is a mistake, and the mistake it invites is the worst available — the flag is
// simply not seen, so the command renders a LIVE session-start block, Obsidian
// sync and session-count bump included, for a request that asked about the past.
// The count would move the present's session number because of a past reading, and
// nothing in the output would say the flag was dropped.
func TestContextAsOfWithoutAValueIsRefused(t *testing.T) {
	for _, argv := range [][]string{
		{"--as-of"},
		{"--cwd", "/tmp", "--as-of"},
		{"--as-of", "--cwd", "/tmp"}, // a flag is not an instant
	} {
		got, err := contextAsOf(argv)
		if err == nil {
			t.Errorf("contextAsOf(%v) = %v, want a refusal: a dropped flag renders the present for a past-instant request", argv, got)
			continue
		}
		if got != nil {
			t.Errorf("contextAsOf(%v) returned %v alongside its error, want nil", argv, got)
		}
		if !strings.Contains(err.Error(), "--as-of") {
			t.Errorf("contextAsOf(%v) refused with %q, want it to name the flag that was incomplete", argv, err)
		}
	}
}

// TestContextAsOfNormalisesToUTC: the rendered note carries the instant, so two
// spellings of one moment must not produce two different labels for the same read.
func TestContextAsOfNormalisesToUTC(t *testing.T) {
	got, err := contextAsOf([]string{"--as-of", "2026-09-20T11:00:00+02:00"})
	if err != nil {
		t.Fatalf("contextAsOf: %v", err)
	}
	if got == nil {
		t.Fatal("contextAsOf returned nil for a valid instant")
	}
	if want := "2026-09-20T09:00:00Z"; got.UTC().Format(time.RFC3339) != want {
		t.Errorf("contextAsOf normalised to %s, want %s", got.UTC().Format(time.RFC3339), want)
	}
}
