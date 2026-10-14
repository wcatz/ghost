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

// The post-upgrade integration wiring check (#1215).
//
// `ghost upgrade` used to run mcpinit.ReportStaleIntegrations in its own
// process after swapping the binary. That process is still the image the run
// STARTED as, and two of the report's checks — the opencode lifecycle plugin
// and the goose plugin package — byte-compare the installed wiring against the
// sources embedded in the image that runs them. So the comparison was always
// against what the OLD release rendered, and drift the new release introduced
// was the one drift the report could never name: a check that exists to catch a
// failure reported success in exactly that case. The check now belongs to the
// binary that was just installed, and these tests hold the call site to it.

// wiringStubReport is what the stand-in for the installed binary prints, so a
// test can tell the child's own output from anything the parent wrote.
const wiringStubReport = "wiring check from the new binary"

// wiringStubFileName is the file name the stand-in must carry for this OS to
// run it: Windows matches names against PATHEXT, so an extensionless file
// cannot be executed there.
func wiringStubFileName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".bat"
	}
	return name
}

// writeWiringStub writes an executable that records the command line it was
// handed and prints wiringStubReport. It stands in for the release an upgrade
// just installed, so the production child-spawn is exercised rather than a
// test double: what the upgrade path has to get right is WHO it runs and with
// WHICH argv, and only a real process answers that.
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

// swapSpawnWiringCheck substitutes the spawner the post-upgrade wiring check
// uses, restoring it when the test ends. The var is the seam because the test
// runner's own binary is not the artifact under test: exec'ing it with the
// wiring subcommand would assert nothing about a release that has none.
func swapSpawnWiringCheck(t *testing.T, spawn func(exe string, out io.Writer) error) {
	t.Helper()
	orig := spawnWiringCheck
	spawnWiringCheck = spawn
	t.Cleanup(func() { spawnWiringCheck = orig })
}

// TestPostUpgradeWiringCheckRunsInTheNewBinary is the property #1215 asked
// for, at the call site: the check runs as a child of the binary that was just
// installed, and its report reaches the same stdout the command reports on.
// A check run in this process would compare against the old image's embedded
// sources, which is the defect.
func TestPostUpgradeWiringCheckRunsInTheNewBinary(t *testing.T) {
	var gotExe string
	swapSpawnWiringCheck(t, func(exe string, out io.Writer) error {
		gotExe = exe
		_, _ = fmt.Fprintf(out, "\nIntegration wiring check:\n  ! opencode lifecycle plugin is missing or outdated — run `ghost mcp init --client opencode` to reinstall it\n")
		return nil
	})

	var out, errOut bytes.Buffer
	reportStaleIntegrations("/opt/ghost/bin/ghost", upgradeStreams{out: &out, err: &errOut})

	if gotExe != "/opt/ghost/bin/ghost" {
		t.Errorf("the wiring check ran against %q, want the binary just installed at /opt/ghost/bin/ghost", gotExe)
	}
	for _, want := range []string{"Integration wiring check:", "opencode lifecycle plugin is missing or outdated"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the child's report did not reach the caller's stdout (want %q), got:\n%s", want, out.String())
		}
	}
	if errOut.Len() != 0 {
		t.Errorf("a check that ran wrote to stderr: %q", errOut.String())
	}
}

// TestPostUpgradeWiringCheckNamesTheCommandWhenTheChildCannotRun is the other
// half of the defect: the original failure was a check that reported success
// in the one case it existed to catch, and a child that could not be spawned
// is that case in a new shape. Silence would be an upgrade reporting success
// while saying nothing about wiring it never checked, so the run says what
// happened and names the command that runs the check by hand.
func TestPostUpgradeWiringCheckNamesTheCommandWhenTheChildCannotRun(t *testing.T) {
	swapSpawnWiringCheck(t, func(exe string, out io.Writer) error {
		return errors.New("exec: \"ghost\": executable file not found in $PATH")
	})

	var out, errOut bytes.Buffer
	reportStaleIntegrations("/gone/ghost", upgradeStreams{out: &out, err: &errOut})

	if out.Len() != 0 {
		t.Errorf("a child that never ran printed to stdout: %q", out.String())
	}
	if !strings.Contains(errOut.String(), "integration wiring check did not run") {
		t.Errorf("stderr %q should say the check did not run, so an unchecked wiring is never silent", errOut.String())
	}
	if !strings.Contains(errOut.String(), "ghost mcp wiring") {
		t.Errorf("stderr %q should name the command that runs the check by hand", errOut.String())
	}
}

// TestSpawnWiringCheckExecsTheInstalledBinaryWithTheWiringSubcommand
// exercises the production spawner itself, against a stand-in for the release
// just installed. Two things are pinned, and only a real process pins them:
// the child is the binary at exe (not this process, which is the old image),
// and the argv is one the dispatch routes — a word nothing answers would exit
// 2 having checked nothing, which is the same silent failure in a new shape.
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
	if argv := strings.TrimSpace(string(got)); argv != "mcp wiring" {
		t.Errorf("the installed binary was run with %q, want the wiring subcommand argv %q", argv, "mcp wiring")
	}
	if !strings.Contains(out.String(), wiringStubReport) {
		t.Errorf("the child's report was not streamed to the caller's writer: %q", out.String())
	}
}

// isolateWiringCheckEnv points every root the wiring check reads at a fresh
// temp directory — HOME, the XDG tree opencode resolves, PATH — and clears the
// client-config overrides it would otherwise inherit from the machine running
// the test, so an invocation reads no real client wiring and reports nothing.
// Returns the roots, so a caller can assert nothing was written.
func isolateWiringCheckEnv(t *testing.T) []string {
	t.Helper()
	roots := isolatedHelpFS(t)
	// codex is judged when its home directory EXISTS, so the override has to
	// name a path that does not: an empty value falls back to ~/.codex under
	// the isolated HOME, which nothing has created.
	t.Setenv("CODEX_HOME", "")
	// opencode's three env layers and its config-directory override: the check
	// judges the file opencode itself resolves, so a value inherited from the
	// host would point it at a real install.
	t.Setenv("OPENCODE_CONFIG_DIR", "")
	t.Setenv("OPENCODE_CONFIG", "")
	t.Setenv("OPENCODE_CONFIG_CONTENT", "")
	return roots
}

// TestMCPWiringIsDispatchedAndItsHelpHasNoSideEffect pins both halves of the
// new subcommand's surface. The dispatch half matters because an unrouted
// word is the #691 usage error — exit 2, having checked nothing, which is the
// same silent failure in a new shape; the run is silent by construction here
// (no client wiring exists in the isolated environment), so exit 0 with empty
// output is the routed answer. The help half is the #630 contract: a help
// request is answered from the usage table and never reaches the check.
func TestMCPWiringIsDispatchedAndItsHelpHasNoSideEffect(t *testing.T) {
	t.Run("the dispatch routes the word to the check", func(t *testing.T) {
		roots := isolateWiringCheckEnv(t)
		resetDetectRemote(t)

		var code int
		stdout, stderr := captureStreams(t, func() { code = runCLI([]string{"mcp", "wiring"}, dispatchCommand) })

		if code != 0 {
			t.Errorf("`ghost mcp wiring` exit code = %d, want 0: the word is routed, and a check that found nothing reports nothing", code)
		}
		if stderr != "" {
			t.Errorf("`ghost mcp wiring` stderr = %q, want no diagnostic: a routed subcommand is not a usage error", stderr)
		}
		if stdout != "" {
			t.Errorf("`ghost mcp wiring` stdout = %q, want silence with no client wiring in use", stdout)
		}
		assertNoFiles(t, roots)
	})

	t.Run("a help request is answered without running the check", func(t *testing.T) {
		isolateWiringCheckEnv(t)
		resetDetectRemote(t)

		for _, tc := range []struct {
			name string
			argv []string
		}{
			{"short flag", []string{"mcp", "wiring", "-h"}},
			{"long flag", []string{"mcp", "wiring", "--help"}},
			{"help command", []string{"help", "mcp", "wiring"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				var code int
				stdout, stderr := captureStreams(t, func() { code = runCLI(tc.argv, dispatchCommand) })

				if code != 0 {
					t.Errorf("`ghost %s` exit code = %d, want 0: a question is not a mistake", strings.Join(tc.argv, " "), code)
				}
				if !strings.HasPrefix(stdout, "Usage: ghost mcp wiring") {
					t.Errorf("`ghost %s` stdout = %q, want the wiring subcommand's own usage", strings.Join(tc.argv, " "), stdout)
				}
				if stderr != "" {
					t.Errorf("`ghost %s` stderr = %q, want the usage on stdout alone", strings.Join(tc.argv, " "), stderr)
				}
			})
		}
	})

	t.Run("an unknown subcommand next to it is still a usage error", func(t *testing.T) {
		isolateWiringCheckEnv(t)
		resetDetectRemote(t)

		var code int
		_, stderr := captureStreams(t, func() { code = runCLI([]string{"mcp", "wiringg"}, dispatchCommand) })

		if code != 2 {
			t.Errorf("`ghost mcp wiringg` exit code = %d, want 2: the word is not one this build routes", code)
		}
		if !strings.Contains(stderr, `ghost mcp: unknown command: "wiringg"`) {
			t.Errorf("`ghost mcp wiringg` stderr = %q, want the unrecognised word named at the mcp level", stderr)
		}
	})
}

// wiringCallName returns the name a call expression invokes — an identifier's own
// name, or a selector's function name — so a source scan can tell a bare
// `reportStaleIntegrations(...)` from `mcpinit.ReportStaleIntegrations(...)`:
// the two differ only in their leading identifier, and it is the difference
// between the child-spawning wrapper and the in-process call this file exists to
// forbid.
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
// which no functional test in this package can reach. runUpgrade exits the
// process on every failure path and reaches GitHub on every success path, so the
// post-upgrade step can only ever be driven through the seam it is written on —
// and the tests above call that seam directly, which is exactly why they cannot
// see who calls it. Restoring the pre-#1215 line
// (mcpinit.ReportStaleIntegrations in the upgrading process) leaves all of them
// green while reintroducing the defect: the comparison would again be against the
// OLD image's embedded sources. This scan is the missing half, and it is the same
// go/ast shape as the package's other structural guards.
//
// It fails in both directions, so it cannot pass by doing nothing: runUpgrade
// must call the local reportStaleIntegrations (the wrapper around
// spawnWiringCheck), and must not call any ReportStaleIntegrations in this
// process. A missing runUpgrade is a failure too, rather than a scan over
// nothing.
func TestRunUpgradeRunsTheWiringCheckThroughTheSeam(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "upgrade.go", nil, 0)
	if err != nil {
		t.Fatalf("parse upgrade.go: %v", err)
	}

	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "runUpgrade" {
			body = fn.Body
			break
		}
	}
	if body == nil {
		t.Fatal("runUpgrade not found in upgrade.go: the post-upgrade wiring check has no call site this guard can hold")
	}

	var callsSeam bool
	var inProcess []string
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch wiringCallName(call) {
		case "reportStaleIntegrations":
			callsSeam = true
		case "ReportStaleIntegrations":
			inProcess = append(inProcess, fset.Position(call.Pos()).String())
		}
		return true
	})

	for _, pos := range inProcess {
		t.Errorf("%s: runUpgrade calls ReportStaleIntegrations in this process, so the embedded-source comparisons judge against the OLD image — the defect #1215 fixed. Run it in the binary just installed, through reportStaleIntegrations", pos)
	}
	if !callsSeam {
		t.Error("runUpgrade does not call reportStaleIntegrations: the post-upgrade wiring check is gone, and an upgrade would report success while checking nothing")
	}
}
