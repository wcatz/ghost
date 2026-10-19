package main

// lifecycle.log is one file shared by every project's detached lifecycle chain,
// so two chains that end close together append to it interleaved. These tests
// hold every line the coordinator itself writes to the shape the hook's
// cooldown-skip line already has: an RFC3339 time, then `lifecycle:`, then the
// project the run is for, rendered through memory.RefusalArg. A run also opens
// with a line naming its project and start time, so the lines that follow can
// be matched to it.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// runLinePrefix matches the prefix every coordinator line carries and captures
// its time and its rendered project.
var runLinePrefix = regexp.MustCompile(`^(\S+) lifecycle: project ("(?:[^"\\]|\\.)*"): `)

// checkRunLine fails the test unless line carries the coordinator prefix with
// an RFC3339 time and the project rendered as want, and returns the rest.
func checkRunLine(t *testing.T, line, want string) string {
	t.Helper()
	m := runLinePrefix.FindStringSubmatch(line)
	if m == nil {
		t.Errorf("coordinator line names no run (want `<RFC3339> lifecycle: project %s: ...`):\n%q", want, line)
		return ""
	}
	if _, err := time.Parse(time.RFC3339, m[1]); err != nil {
		t.Errorf("coordinator line time %q is not RFC3339: %v\n%q", m[1], err, line)
	}
	if m[2] != want {
		t.Errorf("coordinator line project = %s, want %s\n%q", m[2], want, line)
	}
	return line[len(m[0]):]
}

// runLifecycleLines runs the coordinator in-process for project and returns
// the non-empty lines it wrote to stderr. isolatedLifecycleEnv has no harness
// and only reflect enabled, so reflect is skipped and no phase child is ever
// spawned: every line here is the coordinator's own.
func runLifecycleLines(t *testing.T, project string) []string {
	t.Helper()
	isolatedLifecycleEnv(t)
	printed := captureStderr(t, func() {
		runLifecycleWithArgs(t, "--project", project)
	})
	var lines []string
	for _, l := range strings.Split(printed, "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		t.Fatal("the coordinator wrote nothing to stderr")
	}
	return lines
}

// TestRunLifecycleEveryLineNamesItsRun: the scratch reap, the audit sidecar
// sweep and the reflect-skip notice are the lines the issue found carrying no
// project and no time; with them attributed, an interleaved log can be split
// back into its runs.
func TestRunLifecycleEveryLineNamesItsRun(t *testing.T) {
	lines := runLifecycleLines(t, "alpha")
	var bodies []string
	for _, l := range lines {
		bodies = append(bodies, checkRunLine(t, l, `"alpha"`))
	}
	joined := strings.Join(bodies, "\n")
	for _, want := range []string{"scratch reap removed=", "audit sidecar sweep removed=", "skipping reflect"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no attributed %q line among:\n%s", want, strings.Join(lines, "\n"))
		}
	}
}

// TestRunLifecycleOpensWithAStartLine: the first line of a run names its
// project and when it started, so a failure further down can be matched to
// the run it belongs to even when another project's run sits between them.
func TestRunLifecycleOpensWithAStartLine(t *testing.T) {
	before := time.Now().Add(-time.Second)
	lines := runLifecycleLines(t, "alpha")
	body := checkRunLine(t, lines[0], `"alpha"`)
	if !strings.HasPrefix(body, "run started") {
		t.Fatalf("the run does not open with its start line; first line:\n%q", lines[0])
	}
	stamp, err := time.Parse(time.RFC3339, runLinePrefix.FindStringSubmatch(lines[0])[1])
	if err != nil {
		t.Fatalf("start line time: %v", err)
	}
	if stamp.Before(before.Truncate(time.Second)) || stamp.After(time.Now().Add(time.Second)) {
		t.Errorf("start line time %s is not this run's start", stamp)
	}
}

// TestRunLifecycleBoundsAnOddProject: --project is the session's clone URL as
// often as a name, and nothing bounds it on the way in. An oversized operand
// with a newline in it must come out withheld with its length, never repeated,
// and must not split a coordinator line in two.
func TestRunLifecycleBoundsAnOddProject(t *testing.T) {
	odd := "odd project\n" + strings.Repeat("proj", 100)
	lines := runLifecycleLines(t, odd)
	for _, l := range lines {
		if strings.Contains(l, "proj"+"proj"+"proj") {
			t.Errorf("a coordinator line repeated the oversized project:\n%q", l)
		}
		m := runLinePrefix.FindStringSubmatch(l)
		if m == nil {
			t.Errorf("a coordinator line names no run (a newline in the project splits it?):\n%q", l)
			continue
		}
		if !strings.Contains(m[2], "withheld") || !strings.Contains(m[2], "bytes") {
			t.Errorf("the oversized project was not withheld with its length: %s", m[2])
		}
	}
}

// TestLifecycleLineRendersTheProjectAsAToken: the line renderer itself, for
// the phase outcome lines a test cannot reach in-process (a phase is a child
// of the running binary, and under go test that binary is the suite).
func TestLifecycleLineRendersTheProjectAsAToken(t *testing.T) {
	// A local -06:00 time is written in UTC, as the cooldown-skip line is.
	now := time.Date(2026, 10, 17, 17, 7, 9, 0, time.FixedZone("x", -6*3600))
	got := lifecycleLine(now, "beta", "reflect failed after 1s (continuing): exit status 1")
	want := `2026-10-17T23:07:09Z lifecycle: project "beta": reflect failed after 1s (continuing): exit status 1` + "\n"
	if got != want {
		t.Errorf("lifecycleLine =\n%q\nwant\n%q", got, want)
	}

	key := lifecycleCredential()
	for name, project := range map[string]string{
		"token-shaped": key,
		"oversized":    strings.Repeat("proj", 100),
		"newline":      "a\nb",
	} {
		out := lifecycleLine(now, project, "scratch reap removed=0")
		if strings.Count(out, "\n") != 1 || !strings.HasSuffix(out, "\n") {
			t.Errorf("%s: the line is not exactly one line:\n%q", name, out)
		}
		if strings.Contains(out, project) {
			t.Errorf("%s: the raw project reached the line:\n%q", name, out)
		}
		checkRunLine(t, strings.TrimSuffix(out, "\n"), memory.RefusalArg("project", project))
	}
	if out := lifecycleLine(now, key, "x"); !strings.Contains(out, "withheld") {
		t.Errorf("a token-shaped project was not withheld:\n%q", out)
	}
	if out := lifecycleLine(now, strings.Repeat("proj", 100), "x"); !strings.Contains(out, "bytes") {
		t.Errorf("an oversized project was not withheld with its length:\n%q", out)
	}
}

// TestRunLifecycleWritesNoUnattributedLine is the structural half: the phase
// outcome lines ("completed in", "failed after", "interrupted") sit in a loop
// that only runs a child, so this holds every stderr print in the coordinator
// to the run's renderer. The two allowed exceptions are the argument error,
// written before any project is known, and the audit report, a multi-line
// report from internal/audit whose header already names the project.
func TestRunLifecycleWritesNoUnattributedLine(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "lifecycle.go", nil, 0)
	if err != nil {
		t.Fatalf("parse lifecycle.go: %v", err)
	}
	coordinator := map[string]bool{"runLifecycle": true, "runAuditPhase": true}
	found := 0
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || !coordinator[fn.Name.Name] {
			continue
		}
		found++
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) < 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !strings.HasPrefix(sel.Sel.Name, "Fprint") {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "fmt" {
				return true
			}
			w, ok := call.Args[0].(*ast.SelectorExpr)
			if !ok || w.Sel.Name != "Stderr" {
				return true
			}
			if lit, ok := call.Args[1].(*ast.BasicLit); ok && lit.Value == `"error: %v\n"` && fn.Name.Name == "runLifecycle" {
				return true // the argument error: no project exists yet
			}
			if c, ok := call.Args[1].(*ast.CallExpr); ok {
				if s, ok := c.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "String" {
					return true // the audit report
				}
			}
			t.Errorf("%s: %s writes to stderr without naming its run",
				fset.Position(call.Pos()), fn.Name.Name)
			return true
		})
	}
	if found != len(coordinator) {
		t.Fatalf("found %d of the coordinator functions %v", found, coordinator)
	}
}
