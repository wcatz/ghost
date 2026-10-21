package mcpinit

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeClaudeStub installs a `claude` stub that answers `mcp get ghost` with
// the text in the file beside it, and refuses every other command.
//
// The text is read with the shell's own built-ins rather than with `cat`, so
// the stub answers under the empty PATH a status test isolates the host with.
// The Windows form is a real batch file, since PATHEXT governs discovery
// there, and it reads the file with `type` for the same reason.
func writeClaudeStub(t *testing.T, dir string) string {
	t.Helper()
	out := filepath.Join(dir, "get.out")
	if err := os.WriteFile(out, []byte("No MCP server named \"ghost\" found.\n"), 0o644); err != nil {
		t.Fatalf("write the registration file: %v", err)
	}
	name := "claude"
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = mcp ] && [ \"$2\" = get ]; then\n" +
		"  while IFS= read -r line; do printf '%s\\n' \"$line\"; done < '" + out + "'\n" +
		"  exit 0\n" +
		"fi\n" +
		"exit 1\n"
	if runtime.GOOS == "windows" {
		name += ".bat"
		script = "@echo off\r\n" +
			"if \"%1\"==\"mcp\" if \"%2\"==\"get\" goto do_get\r\n" +
			"exit /b 1\r\n" +
			":do_get\r\n" +
			"type \"" + out + "\"\r\n" +
			"exit /b 0\r\n"
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write the claude stub: %v", err)
	}
	return path
}

// claudeRegistrationFixture installs the two binaries a Claude Code status run
// needs: a `ghost` stub the run resolves as the ghost binary, and a `claude`
// stub whose `mcp get ghost` answers with the registration the caller writes.
// It returns the resolved ghost path and the directory holding the claude
// stub, which is where the registration text goes.
func claudeRegistrationFixture(t *testing.T) (ghostPath, claudeDir string) {
	t.Helper()
	binDir := t.TempDir()
	ghostPath = writeStub(t, binDir, "ghost")
	claudeDir = t.TempDir()
	writeClaudeStub(t, claudeDir)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+claudeDir)
	return ghostPath, claudeDir
}

// claudeRegistrationText renders the `claude mcp get ghost` output for a stdio
// registration of command with the given arguments — the rendering the real
// client prints.
func claudeRegistrationText(command, args string) string {
	return "ghost:\n  Scope: User\n  Type: stdio\n  Command: " + command + "\n  Args: " + args + "\n"
}

// TestStatus_ClaudeMCPRegistration pins the judgement the status check makes
// about Claude Code's MCP registration: the registration is judged on WHAT IT
// RUNS, so an entry left pointing at a moved, deleted or unrelated program is
// reported as drift rather than as a working registration, and the
// registration ghost itself writes still reads as registered.
func TestStatus_ClaudeMCPRegistration(t *testing.T) {
	t.Run("a registration running another program is drift", func(t *testing.T) {
		statusEnv(t)
		otherDir := t.TempDir()
		otherGhost := writeStub(t, otherDir, "ghost")
		_, claudeDir := claudeRegistrationFixture(t)
		writeRegistration(t, claudeDir, claudeRegistrationText(otherGhost, "mcp"))

		var out bytes.Buffer
		if _, err := Status(&out); err != nil {
			t.Fatalf("Status: %v", err)
		}
		line := statusLineContaining(out.String(), "MCP server")
		if !strings.Contains(line, "✗") {
			t.Fatalf("a registration running another program must not read as registered, got %q", line)
		}
		if !strings.Contains(line, otherGhost) {
			t.Errorf("the failed line must name the command the registration runs, got %q", line)
		}
	})

	t.Run("a registration running a program that is gone is drift", func(t *testing.T) {
		statusEnv(t)
		_, claudeDir := claudeRegistrationFixture(t)
		gone := filepath.Join(t.TempDir(), "ghost")
		writeRegistration(t, claudeDir, claudeRegistrationText(gone, "mcp"))

		var out bytes.Buffer
		if _, err := Status(&out); err != nil {
			t.Fatalf("Status: %v", err)
		}
		line := statusLineContaining(out.String(), "MCP server")
		if !strings.Contains(line, "✗") {
			t.Fatalf("a registration whose program is gone must not read as registered, got %q", line)
		}
		if !strings.Contains(line, gone) {
			t.Errorf("the failed line must name the command the registration runs, got %q", line)
		}
	})

	t.Run("a registration with the wrong arguments is drift", func(t *testing.T) {
		statusEnv(t)
		ghostPath, claudeDir := claudeRegistrationFixture(t)
		writeRegistration(t, claudeDir, claudeRegistrationText(ghostPath, "mcp --not-a-flag"))

		var out bytes.Buffer
		if _, err := Status(&out); err != nil {
			t.Fatalf("Status: %v", err)
		}
		line := statusLineContaining(out.String(), "MCP server")
		if !strings.Contains(line, "✗") {
			t.Fatalf("a registration with the wrong arguments must not read as registered, got %q", line)
		}
		if !strings.Contains(line, "mcp --not-a-flag") {
			t.Errorf("the failed line must name the arguments the registration runs, got %q", line)
		}
	})

	t.Run("a registration running this ghost is registered", func(t *testing.T) {
		statusEnv(t)
		ghostPath, claudeDir := claudeRegistrationFixture(t)
		writeRegistration(t, claudeDir, claudeRegistrationText(ghostPath, "mcp"))

		var out bytes.Buffer
		if _, err := Status(&out); err != nil {
			t.Fatalf("Status: %v", err)
		}
		line := statusLineContaining(out.String(), "MCP server")
		if !strings.Contains(line, "✓") {
			t.Fatalf("a registration running the resolved ghost must read as registered, got %q", line)
		}
	})

	t.Run("no registration is not registered", func(t *testing.T) {
		statusEnv(t)
		_, claudeDir := claudeRegistrationFixture(t)
		writeRegistration(t, claudeDir, "No MCP server named \"ghost\" found.\n")

		var out bytes.Buffer
		if _, err := Status(&out); err != nil {
			t.Fatalf("Status: %v", err)
		}
		line := statusLineContaining(out.String(), "MCP server")
		if !strings.Contains(line, "✗") {
			t.Fatalf("a client holding no registration must not read as registered, got %q", line)
		}
		if !strings.Contains(line, "not registered") {
			t.Errorf("the failed line must say the server is not registered, got %q", line)
		}
	})
}
