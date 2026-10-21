package mcpinit

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// claudeInitFixture installs the binaries the installer registers: a `ghost`
// stub it would write into the registration, a name-sibling beside it, and a
// `claude` stub that answers `mcp get ghost` with the registration the caller
// writes and logs every command it is asked to run.
func claudeInitFixture(t *testing.T) (ghostBin, sibling, claudeBin, dir string) {
	t.Helper()
	binDir := t.TempDir()
	ghostBin = writeStub(t, binDir, "ghost")
	sibling = writeStub(t, binDir, "ghost-0.54.0")
	dir = t.TempDir()
	claudeBin = writeFakeClaude(t, dir, claudeFake{})
	return ghostBin, sibling, claudeBin, dir
}

// claudeCallLog reads the fake's log of every command line it was asked to
// run, one per line.
func claudeCallLog(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read the fake's %s log: %v", name, err)
	}
	return string(b)
}

// TestRegisterMCPRepairsARegistrationThatIsNotGhosts pins the installer's own
// judgement of the registration it finds: one that does not run this build's
// ghost binary, or does not run it with the contract's arguments, is drift
// the installer REPAIRS. It takes the remove-then-add path and reports the
// registration as updated, so the command the status report names as the
// repair actually repairs.
//
// The four shapes here are the ones the installer's old judgement accepted as
// its own: a foreign command with the ghost path somewhere else in the
// rendering, a command that only BEGINS with the ghost path, the wrong
// arguments, and no arguments at all.
func TestRegisterMCPRepairsARegistrationThatIsNotGhosts(t *testing.T) {
	cases := []struct {
		name         string
		registration func(ghostBin, sibling string) string
	}{
		{
			name: "a foreign command with the ghost path in the arguments",
			registration: func(ghostBin, _ string) string {
				return "ghost:\n  Scope: User\n  Type: stdio\n  Command: /usr/bin/othertool\n" +
					"  Args: " + ghostBin + " mcp\n"
			},
		},
		{
			name: "a command that only begins with the ghost path",
			registration: func(_, sibling string) string {
				return claudeRegistrationText(sibling, "mcp")
			},
		},
		{
			name: "the wrong arguments",
			registration: func(ghostBin, _ string) string {
				return claudeRegistrationText(ghostBin, "serve")
			},
		},
		{
			name: "no arguments at all",
			registration: func(ghostBin, _ string) string {
				return "ghost:\n  Scope: User\n  Type: stdio\n  Command: " + ghostBin + "\n"
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ghostBin, sibling, claudeBin, dir := claudeInitFixture(t)
			writeRegistration(t, dir, tc.registration(ghostBin, sibling))

			var out bytes.Buffer
			if err := registerMCP(&out, ghostBin, claudeBin, false); err != nil {
				t.Fatalf("registerMCP: %v", err)
			}

			calls := claudeCallLog(t, dir, "calls")
			if !strings.Contains(calls, "mcp remove") {
				t.Errorf("the registration that is not ghost's must be removed, the calls were:\n%s", calls)
			}
			if !strings.Contains(calls, "mcp add-json") {
				t.Errorf("the registration must be added again, the calls were:\n%s", calls)
			}
			if strings.Contains(out.String(), "already registered") {
				t.Errorf("a registration that is not ghost's must not be reported as registered, got:\n%s", out.String())
			}
			if !strings.Contains(out.String(), "updated ghost MCP server") {
				t.Errorf("the run must report the registration as updated, got:\n%s", out.String())
			}
			// The registration it wrote is the contract's own: this build's
			// binary with the `mcp` argument.
			// The registration travels as JSON, so the path is compared in
			// its JSON form (a Windows path's backslashes are escaped there),
			// without its quotes: the Windows stub logs them escaped again.
			added := claudeCallLog(t, dir, "adds")
			quotedBin, _ := json.Marshal(ghostBin)
			if !strings.Contains(added, strings.Trim(string(quotedBin), `"`)) {
				t.Errorf("the added registration must run this build's binary, got %q", added)
			}
			if !strings.Contains(added, "mcp") {
				t.Errorf("the added registration must carry the mcp argument, got %q", added)
			}
		})
	}
}

// TestRegisterMCPLeavesTheCurrentRegistrationAlone is the other side: the
// registration this build installs is left exactly as it is — the client is
// asked for it and nothing else — and a symlink onto the same binary is that
// same registration, because the identity of the command is the file rather
// than its spelling.
func TestRegisterMCPLeavesTheCurrentRegistrationAlone(t *testing.T) {
	t.Run("the exact current registration", func(t *testing.T) {
		ghostBin, _, claudeBin, dir := claudeInitFixture(t)
		writeRegistration(t, dir, claudeRegistrationText(ghostBin, "mcp"))

		var out bytes.Buffer
		if err := registerMCP(&out, ghostBin, claudeBin, false); err != nil {
			t.Fatalf("registerMCP: %v", err)
		}
		if !strings.Contains(out.String(), "already registered") {
			t.Errorf("the current registration must be reported as registered, got:\n%s", out.String())
		}
		calls := claudeCallLog(t, dir, "calls")
		if !strings.Contains(calls, "mcp get") {
			t.Errorf("the client must be asked for its registration, the calls were:\n%s", calls)
		}
		if strings.Contains(calls, "mcp remove") {
			t.Errorf("the current registration must not be removed, the calls were:\n%s", calls)
		}
		if strings.Contains(calls, "mcp add-json") {
			t.Errorf("the current registration must not be added again, the calls were:\n%s", calls)
		}
	})

	t.Run("a symlink onto the binary", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("creating a symlink on Windows needs Developer Mode or elevated rights")
		}
		ghostBin, _, claudeBin, dir := claudeInitFixture(t)
		link := filepath.Join(t.TempDir(), "ghost-link")
		if err := os.Symlink(ghostBin, link); err != nil {
			t.Fatalf("link the ghost binary: %v", err)
		}
		writeRegistration(t, dir, claudeRegistrationText(link, "mcp"))

		var out bytes.Buffer
		if err := registerMCP(&out, ghostBin, claudeBin, false); err != nil {
			t.Fatalf("registerMCP: %v", err)
		}
		if !strings.Contains(out.String(), "already registered") {
			t.Errorf("a symlink onto this build's binary must read as registered, got:\n%s", out.String())
		}
		calls := claudeCallLog(t, dir, "calls")
		if strings.Contains(calls, "mcp remove") || strings.Contains(calls, "mcp add-json") {
			t.Errorf("a symlink onto this build's binary must not be re-registered, the calls were:\n%s", calls)
		}
	})
}

// writeRepairingClaudeStub installs a `claude` stub that keeps the
// registration the way a client does: `mcp get ghost` answers with the file
// beside it, `mcp remove` empties it, and `mcp add-json` writes the
// registration this build installs. A test can therefore run the install and
// then read back what the client would hold.
//
// The file is read with the shell's own built-ins rather than with `cat`, so
// the stub answers under the empty PATH a status test isolates the host with.
// The Windows form is a real batch file, since PATHEXT governs discovery
// there, and it reads the file with `type` for the same reason.
func writeRepairingClaudeStub(t *testing.T, dir, ghostBin string) string {
	t.Helper()
	out := filepath.Join(dir, "get.out")
	if err := os.WriteFile(out, []byte("No MCP server named \"ghost\" found.\n"), 0o644); err != nil {
		t.Fatalf("write the registration file: %v", err)
	}
	name := "claude"
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> '" + filepath.Join(dir, "calls") + "'\n" +
		"if [ \"$1\" = mcp ] && [ \"$2\" = get ]; then\n" +
		"  while IFS= read -r line; do printf '%s\\n' \"$line\"; done < '" + out + "'\n" +
		"  exit 0\n" +
		"fi\n" +
		"if [ \"$1\" = mcp ] && [ \"$2\" = remove ]; then\n" +
		"  : > '" + out + "'\n" +
		"  exit 0\n" +
		"fi\n" +
		"if [ \"$1\" = mcp ] && [ \"$2\" = add-json ]; then\n" +
		"  printf 'ghost:\\n  Scope: User\\n  Type: stdio\\n  Command: %s\\n  Args: mcp\\n' '" + ghostBin + "' > '" + out + "'\n" +
		"  exit 0\n" +
		"fi\n" +
		"exit 1\n"
	if runtime.GOOS == "windows" {
		name += ".bat"
		script = "@echo off\r\n" +
			">>\"" + filepath.Join(dir, "calls") + "\" echo %*\r\n" +
			"if \"%1\"==\"mcp\" if \"%2\"==\"get\" goto do_get\r\n" +
			"if \"%1\"==\"mcp\" if \"%2\"==\"remove\" goto do_remove\r\n" +
			"if \"%1\"==\"mcp\" if \"%2\"==\"add-json\" goto do_add\r\n" +
			"exit /b 1\r\n" +
			":do_get\r\n" +
			"if exist \"" + out + "\" type \"" + out + "\"\r\n" +
			"exit /b 0\r\n" +
			":do_remove\r\n" +
			"type nul > \"" + out + "\"\r\n" +
			"exit /b 0\r\n" +
			":do_add\r\n" +
			"> \"" + out + "\" echo ghost:\r\n" +
			">> \"" + out + "\" echo   Scope: User\r\n" +
			">> \"" + out + "\" echo   Type: stdio\r\n" +
			">> \"" + out + "\" echo   Command: " + ghostBin + "\r\n" +
			">> \"" + out + "\" echo   Args: mcp\r\n" +
			"exit /b 0\r\n"
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write the claude stub: %v", err)
	}
	return path
}

// TestStatusThenInitThenStatusRepairsTheRegistration closes the loop the
// status report promises: the drift it reports is repaired by the command it
// names, so the second status run finds the registration current. The stub
// keeps the registration the way a client does — the add-json writes it — so
// the second run reads what init wrote, not what the test wrote.
func TestStatusThenInitThenStatusRepairsTheRegistration(t *testing.T) {
	statusEnv(t)
	binDir := t.TempDir()
	ghostBin := writeStub(t, binDir, "ghost")
	claudeDir := t.TempDir()
	writeRepairingClaudeStub(t, claudeDir, ghostBin)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+claudeDir)
	// A registration of the right binary with the wrong arguments: the drift
	// the status report names the repair for.
	writeRegistration(t, claudeDir, claudeRegistrationText(ghostBin, "serve"))

	var first bytes.Buffer
	if _, err := Status(&first); err != nil {
		t.Fatalf("Status: %v", err)
	}
	line := statusLineContaining(first.String(), "MCP server")
	if !strings.Contains(line, "✗") {
		t.Fatalf("a registration with the wrong arguments must be reported as drift, got %q", line)
	}
	if !strings.Contains(line, claudeRegistrationRepair) {
		t.Errorf("the failed line must name the repair, got %q", line)
	}

	// The repair the failed line names: `ghost mcp init --client claude`.
	var initOut bytes.Buffer
	if err := Run(&initOut, false); err != nil {
		t.Fatalf("init: %v\n%s", err, initOut.String())
	}

	var second bytes.Buffer
	if _, err := Status(&second); err != nil {
		t.Fatalf("Status: %v", err)
	}
	line = statusLineContaining(second.String(), "MCP server")
	if !strings.Contains(line, "✓") {
		t.Fatalf("the repair must leave the registration current, got %q", line)
	}
}
