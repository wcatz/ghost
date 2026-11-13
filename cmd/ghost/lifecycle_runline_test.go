package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
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

// runLifecycleWithArgs runs the coordinator in-process with the given argv.
func runLifecycleWithArgs(t *testing.T, argv ...string) {
	t.Helper()
	orig := os.Args
	t.Cleanup(func() { os.Args = orig })
	os.Args = append([]string{orig[0], "lifecycle"}, argv...)
	runLifecycle()
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
	for _, want := range []string{"scratch reap removed=", "skipping reflect"} {
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

// TestRunLifecycleWritesNoUnattributedLine is the structural half: the phase
// outcome lines ("completed in", "failed after", "interrupted") sit in a loop
// that only runs a child, so this holds every stderr print in the coordinator
// to the run's renderer. The two allowed exceptions are the argument error,
// written before any project is known, and the multi-line report, whose
// header already names the project.
func TestRunLifecycleWritesNoUnattributedLine(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "lifecycle.go", nil, 0)
	if err != nil {
		t.Fatalf("parse lifecycle.go: %v", err)
	}
	coordinator := map[string]bool{"runLifecycle": true}
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
