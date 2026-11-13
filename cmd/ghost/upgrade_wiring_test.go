package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The post-upgrade integration wiring check (#1215): `ghost upgrade` used to
// run mcpinit.ReportStaleIntegrations in its own (OLD image) process, whose
// embedded-source comparisons could never name drift the new release introduced.
// The check now belongs to the binary just installed, and these tests hold the
// call site to it.

// wiringStubReport is what the stand-in for the installed binary prints, so a
// test can tell the child's own output from the parent's.
const wiringStubReport = "wiring check from the new binary"

// wiringStubFileName is the file name the stand-in must carry for this OS to run
// it (Windows matches names against PATHEXT).
func wiringStubFileName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".bat"
	}
	return name
}

// writeWiringStub writes an executable that records the command line it was
// handed and prints wiringStubReport, standing in for the release just installed
// so the production child-spawn is exercised: WHO it runs and with WHICH argv.
func writeWiringStub(t *testing.T, exe, argvLog string) {
	t.Helper()
	var body string
	if runtime.GOOS == "windows" {
		body = "@echo off\r\necho %* > \"" + argvLog + "\"\r\necho " + wiringStubReport + "\r\n"
	} else {
		body = "#!/bin/sh\nprintf '%s\\n' \"$*\" > '" + argvLog + "'\nprintf '%s\\n' '" + wiringStubReport + "'\n"
	}
	if err := os.WriteFile(exe, []byte(body), 0o755); err != nil {
		t.Fatalf("write the stand-in binary: %v", err)
	}
}

// swapSpawnWiringCheck substitutes the spawner the post-upgrade check uses (the
// test runner's own binary is not the artifact under test).
func swapSpawnWiringCheck(t *testing.T, spawn func(exe string, out io.Writer) error) {
	t.Helper()
	orig := spawnWiringCheck
	spawnWiringCheck = spawn
	t.Cleanup(func() { spawnWiringCheck = orig })
}

// TestPostUpgradeWiringCheckRunsInTheNewBinary is the property #1215 asked for:
// the check runs as a child of the binary just installed and its report reaches
// the command's stdout; a child that cannot run is said so on stderr, naming the
// command that runs the check by hand (silence would report success for wiring
// never checked).
func TestPostUpgradeWiringCheckRunsInTheNewBinary(t *testing.T) {
	var gotExe string
	swapSpawnWiringCheck(t, func(exe string, out io.Writer) error {
		gotExe = exe
		_, _ = fmt.Fprintf(out, "\nIntegration wiring check:\n  ! opencode lifecycle plugin is missing or outdated\n")
		return nil
	})
	var out, errOut bytes.Buffer
	reportStaleIntegrations("/opt/ghost/bin/ghost", upgradeStreams{out: &out, err: &errOut})
	if gotExe != "/opt/ghost/bin/ghost" || !strings.Contains(out.String(), "opencode lifecycle plugin is missing or outdated") || errOut.Len() != 0 {
		t.Errorf("ran %q, stdout %q, stderr %q, want the installed binary's report on stdout alone", gotExe, out.String(), errOut.String())
	}

	swapSpawnWiringCheck(t, func(string, io.Writer) error {
		return errors.New("exec: \"ghost\": executable file not found in $PATH")
	})
	out.Reset()
	errOut.Reset()
	reportStaleIntegrations("/gone/ghost", upgradeStreams{out: &out, err: &errOut})
	if out.Len() != 0 || !strings.Contains(errOut.String(), "integration wiring check did not run") || !strings.Contains(errOut.String(), "ghost mcp wiring") {
		t.Errorf("a child that could not run: stdout %q, stderr %q", out.String(), errOut.String())
	}
}

// TestSpawnWiringCheckExecsTheInstalledBinaryWithTheWiringSubcommand exercises
// the production spawner against a stand-in: the child is the binary at exe (not
// this old image) and the argv is one the dispatch routes.
func TestSpawnWiringCheckExecsTheInstalledBinaryWithTheWiringSubcommand(t *testing.T) {
	dir := t.TempDir()
	argvLog := filepath.Join(dir, "argv")
	exe := filepath.Join(dir, wiringStubFileName("ghost"))
	writeWiringStub(t, exe, argvLog)
	var out bytes.Buffer
	if err := spawnWiringCheck(exe, &out); err != nil {
		t.Fatalf("spawnWiringCheck(%q): %v", exe, err)
	}
	got, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("read the stand-in's argv log: %v", err)
	}
	if argv := strings.TrimSpace(string(got)); argv != "mcp wiring" || !strings.Contains(out.String(), wiringStubReport) {
		t.Errorf("run with %q, report %q, want argv %q and the child's report streamed", argv, out.String(), "mcp wiring")
	}
}

// isolateWiringCheckEnv points every root the wiring check reads at a fresh temp
// directory and clears the client-config overrides it would inherit from the
// machine (codex is judged when its home EXISTS, so CODEX_HOME is emptied to fall
// back to a path nothing created). Returns the roots.
func isolateWiringCheckEnv(t *testing.T) []string {
	t.Helper()
	roots := isolatedHelpFS(t)
	t.Setenv("CODEX_HOME", "")
	t.Setenv("OPENCODE_CONFIG_DIR", "")
	t.Setenv("OPENCODE_CONFIG", "")
	t.Setenv("OPENCODE_CONFIG_CONTENT", "")
	return roots
}

// TestMCPWiringIsDispatchedAndItsHelpHasNoSideEffect: an unrouted word is the
// #691 usage error, so the dispatch must route `mcp wiring` (silent with no
// client wiring in the isolated environment: exit 0, empty output), a help
// request (#630) is answered from the usage table without running the check, and
// a near-miss word is still a usage error.
func TestMCPWiringIsDispatchedAndItsHelpHasNoSideEffect(t *testing.T) {
	roots := isolateWiringCheckEnv(t)
	resetDetectRemote(t)
	run := func(argv ...string) (int, string, string) {
		var code int
		stdout, stderr := captureStreams(t, func() { code = runCLI(argv, dispatchCommand) })
		return code, stdout, stderr
	}
	if code, stdout, stderr := run("mcp", "wiring"); code != 0 || stdout != "" || stderr != "" {
		t.Errorf("`ghost mcp wiring` = exit %d, stdout %q, stderr %q, want a routed, silent run", code, stdout, stderr)
	}
	assertNoFiles(t, roots)
	for _, argv := range [][]string{{"mcp", "wiring", "-h"}, {"mcp", "wiring", "--help"}, {"help", "mcp", "wiring"}} {
		if code, stdout, stderr := run(argv...); code != 0 || !strings.HasPrefix(stdout, "Usage: ghost mcp wiring") || stderr != "" {
			t.Errorf("`ghost %s` = exit %d, stdout %q, stderr %q, want the wiring usage on stdout alone", strings.Join(argv, " "), code, stdout, stderr)
		}
	}
	if code, _, stderr := run("mcp", "wiringg"); code != 2 || !strings.Contains(stderr, `ghost mcp: unknown command: "wiringg"`) {
		t.Errorf("`ghost mcp wiringg` = exit %d, stderr %q, want a usage error naming the word", code, stderr)
	}
}

// wiringCallName returns the name a call expression invokes (an identifier or a
// selector's function name), so a scan can tell the bare `reportStaleIntegrations`
// wrapper from `mcpinit.ReportStaleIntegrations`.
func wiringCallName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	}
	return ""
}

// TestRunUpgradeRunsTheWiringCheckThroughTheSeam holds the call site itself,
// which no functional test can reach (runUpgrade exits the process and reaches
// GitHub): restoring the pre-#1215 in-process call leaves every test above green
// while reintroducing the defect. It fails in both directions: runUpgrade must
// call the local reportStaleIntegrations and must not call any
// ReportStaleIntegrations in this process.
func TestRunUpgradeRunsTheWiringCheckThroughTheSeam(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "upgrade.go", nil, 0)
	if err != nil {
		t.Fatalf("parse upgrade.go: %v", err)
	}
	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "runUpgrade" {
			body = fn.Body
			break
		}
	}
	if body == nil {
		t.Fatal("runUpgrade not found in upgrade.go: the post-upgrade wiring check has no call site this guard can hold")
	}
	var callsSeam bool
	ast.Inspect(body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			switch wiringCallName(call) {
			case "reportStaleIntegrations":
				callsSeam = true
			case "ReportStaleIntegrations":
				t.Errorf("%s: runUpgrade calls ReportStaleIntegrations in this process, so the comparisons judge against the OLD image (#1215); run it in the installed binary through reportStaleIntegrations", fset.Position(call.Pos()))
			}
		}
		return true
	})
	if !callsSeam {
		t.Error("runUpgrade does not call reportStaleIntegrations: an upgrade would report success while checking nothing")
	}
}
