package main

// The bound on one consolidation's input, at the command and at the lifecycle
// phase. A manual `ghost reflect` above the bound refuses and says what to do;
// the lifecycle's reflect phase, which runs the same command with --apply and
// --skip-unchanged, reports a skip instead of failing, so a project past the
// bound does not raise the maintenance alert on every run.
//
// The counts are literals, never maxConsolidationInput plus or minus one: a
// test that derives its counts from the constant moves with it, and then it
// pins nothing about where the bound is.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/mcpinit"
	"github.com/wcatz/ghost/internal/memory"
)

// TestConsolidationBoundIsTwoThousand pins where the bound sits and what each
// side of it does, without a store: 2000 runs in every form, and 2001 is a
// refusal for a manual run and a reported skip for the lifecycle's form.
// Raising the constant fails the 2001 cases.
func TestConsolidationBoundIsTwoThousand(t *testing.T) {
	cases := []struct {
		name                 string
		n                    int
		skipUnchanged, apply bool
		wantLine, wantRefuse bool
		wantPrefix           string
	}{
		{"2000 manual", 2000, false, false, false, false, ""},
		{"2000 lifecycle form", 2000, true, true, false, false, ""},
		{"2001 manual dry run", 2001, false, false, true, true, "error: "},
		{"2001 manual apply", 2001, false, true, true, true, "error: "},
		{"2001 skip-unchanged without apply", 2001, true, false, true, true, "error: "},
		{"2001 lifecycle form", 2001, true, true, true, false, "reflect: skipping"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line, refuse := consolidationBound("proj", tc.n, tc.skipUnchanged, tc.apply)
			if (line != "") != tc.wantLine || refuse != tc.wantRefuse {
				t.Fatalf("consolidationBound(%d) = (%q, %v), want a line %v and refuse %v",
					tc.n, line, refuse, tc.wantLine, tc.wantRefuse)
			}
			if !tc.wantLine {
				return
			}
			if !strings.HasPrefix(line, tc.wantPrefix) {
				t.Errorf("line %q does not start with %q", line, tc.wantPrefix)
			}
			if !strings.Contains(line, "2001 consolidatable memories, above the 2000 limit") ||
				!strings.Contains(line, "at most 2000") {
				t.Errorf("line does not name the count and the bound: %q", line)
			}
			if strings.Count(line, "\n") != 1 || !strings.HasSuffix(line, "\n") {
				t.Errorf("line is not one line: %q", line)
			}
		})
	}
}

// seedConsolidatableRows writes n consolidatable rows (source mcp, unpinned,
// unresolved, project tier) for project into the store at dbPath, by raw SQL
// in one transaction so a few thousand rows cost well under a second. The rows
// are backdated an hour so none reads as a save that raced the run.
func seedConsolidatableRows(t *testing.T, dbPath, project string, n int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	s := memory.NewStore(db, nil)
	if err := s.EnsureProject(context.Background(), project, "/nonexistent/"+project, project); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := 0; i < n; i++ {
		if _, err := tx.Exec(
			`INSERT INTO memories (project_id, category, content, source, importance, created_at, updated_at)
			 VALUES (?, 'fact', ?, 'mcp', 0.6, datetime('now', '-1 hour'), datetime('now', '-1 hour'))`,
			project, fmt.Sprintf("relay %05d listens on its own port", i)); err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}
}

// buildGhostForBoundTest builds the command into a temp dir. A test that needs
// the program builds it; it never re-runs its own binary.
func buildGhostForBoundTest(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "ghost")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// TestReflectRefusesAboveTheInputBoundAndRunsAtIt drives the built binary
// with the offline tier, so no harness is involved: 2001 consolidatable rows
// are refused with a message that says what to do, and 2000 are consolidated
// (a dry run, so the refusal and the run differ only in the count).
func TestReflectRefusesAboveTheInputBoundAndRunsAtIt(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the ghost binary")
	}
	bin := buildGhostForBoundTest(t)
	const project = "bounded"

	t.Run("2001 refused", func(t *testing.T) {
		home := t.TempDir()
		seedConsolidatableRows(t, filepath.Join(home, "data", "ghost", "ghost.db"), project, 2001)
		code, stderr := reflectBinaryRun(t, bin, home, project, "--tier", "sqlite")
		if code != 1 {
			t.Fatalf("exit = %d, want 1; stderr:\n%s", code, stderr)
		}
		for _, want := range []string{
			"has 2001 consolidatable memories, above the 2000 limit",
			"nothing written",
			"ghost resolve",
			"ghost prune",
		} {
			if !strings.Contains(stderr, want) {
				t.Errorf("the refusal does not say %q:\n%s", want, stderr)
			}
		}
	})

	t.Run("2000 runs", func(t *testing.T) {
		home := t.TempDir()
		seedConsolidatableRows(t, filepath.Join(home, "data", "ghost", "ghost.db"), project, 2000)
		code, stderr := reflectBinaryRun(t, bin, home, project, "--tier", "sqlite")
		if code != 0 {
			t.Fatalf("exit = %d, want 0 at the bound itself; stderr:\n%s", code, stderr)
		}
		if strings.Contains(stderr, "limit for a single consolidation") {
			t.Errorf("a project at the bound was refused:\n%s", stderr)
		}
	})
}

// TestLifecycleReflectAboveTheInputBoundIsASkip runs `ghost lifecycle` as
// the Stop hook does, with a reachable (fake) harness so the reflect phase is
// scheduled. A project above the bound must not fail the phase: no failure
// marker is left (an earlier one is cleared, since the run is not a failure),
// the output carries one line saying why the phase skipped and what to do,
// and the harness is never asked to consolidate.
func TestLifecycleReflectAboveTheInputBoundIsASkip(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the ghost binary")
	}
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake harness requires a POSIX shell")
	}
	bin := buildGhostForBoundTest(t)
	dataHome := isolatedLifecycleEnv(t)
	home := filepath.Dir(dataHome)
	const project = "bounded"
	seedConsolidatableRows(t, filepath.Join(dataHome, "ghost", "ghost.db"), project, 2001)

	// A marker from an earlier failed run, so the assertion below is about
	// what this run records rather than about a file that never existed.
	if err := mcpinit.WriteLifecycleFailure(project, []string{"reflect"}, "an earlier failure"); err != nil {
		t.Fatalf("write earlier marker: %v", err)
	}
	marker := filepath.Join(dataHome, "ghost", mcpinit.LifecycleFailureMarkerFile(project))
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the earlier marker was not written where the test reads it: %v", err)
	}

	calls := filepath.Join(t.TempDir(), "calls")
	harness := fakeReflectHarness(t, calls, "x,y", "merged", "")

	cmd := exec.Command(bin, "lifecycle", "--project", project, "--source", "claude-code")
	cmd.Env = []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
		"XDG_DATA_HOME=" + dataHome,
		"PATH=" + t.TempDir(),
		// The coordinator sweeps stale audit sidecars out of os.TempDir(),
		// so the child gets a temp dir of its own rather than the machine's.
		"TMPDIR=" + t.TempDir(),
		"GHOST_OLLAMA_URL=http://127.0.0.1:1",
		"GHOST_CLI_CLAUDE_BINARY=" + harness,
		"GHOST_CLI_OPENCODE_BINARY=" + filepath.Join(home, "no-such-opencode"),
		"GHOST_CLI_CODEX_BINARY=" + filepath.Join(home, "no-such-codex"),
		"GHOST_CLI_GOOSE_BINARY=" + filepath.Join(home, "no-such-goose"),
	}
	if v, ok := os.LookupEnv("GHOST_DEV_FORBID_DATA_DIR"); ok {
		cmd.Env = append(cmd.Env, "GHOST_DEV_FORBID_DATA_DIR="+v)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("lifecycle: %v\n%s", err, out.String())
	}
	got := out.String()

	if strings.Contains(got, "reflect failed") {
		t.Errorf("the reflect phase failed above the bound:\n%s", got)
	}
	if !strings.Contains(got, `lifecycle: project "bounded": reflect completed`) {
		t.Errorf("the reflect phase did not complete:\n%s", got)
	}
	for _, want := range []string{
		"reflect: skipping",
		"has 2001 consolidatable memories, above the 2000 limit",
		"not a failure",
		"ghost resolve",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the phase output does not say %q:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "reflect: skipping"); n != 1 {
		t.Errorf("the skip is reported %d times, want once:\n%s", n, got)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		b, _ := os.ReadFile(marker) //nolint:gosec // a path this test chose
		t.Errorf("a failure marker is present after the run (stat err = %v):\n%s", err, b)
	}
	if n := countLines(t, calls); n != 0 {
		t.Errorf("the harness answered %d consolidation turn(s), want none above the bound", n)
	}
}
