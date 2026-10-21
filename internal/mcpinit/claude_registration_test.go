package mcpinit

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// claudeRegistrationRepair is the repair every drift line must name: the one
// command that re-registers Claude Code's ghost server with this build's
// binary. It is spelled out here rather than read from the package, so a
// change to the message cannot quietly change the expectation with it.
const claudeRegistrationRepair = "run ghost mcp init --client claude"

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
// registration ghost itself writes still reads as registered. Every drift line
// names the repair as well as the drift.
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
		if !strings.Contains(line, claudeRegistrationRepair) {
			t.Errorf("the failed line must name the repair, got %q", line)
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
		if !strings.Contains(line, claudeRegistrationRepair) {
			t.Errorf("the failed line must name the repair, got %q", line)
		}
	})

	t.Run("a registration running a sibling of the ghost's name is drift", func(t *testing.T) {
		statusEnv(t)
		ghostPath, claudeDir := claudeRegistrationFixture(t)
		// A program whose name BEGINS with the ghost's name, beside it: a
		// versioned build the registration still points at. It exists and it
		// runs, so only the identity of the file can reject it.
		sibling := writeStub(t, filepath.Dir(ghostPath), "ghost-0.54.0")
		writeRegistration(t, claudeDir, claudeRegistrationText(sibling, "mcp"))

		var out bytes.Buffer
		if _, err := Status(&out); err != nil {
			t.Fatalf("Status: %v", err)
		}
		line := statusLineContaining(out.String(), "MCP server")
		if !strings.Contains(line, "✗") {
			t.Fatalf("a registration running the ghost's name-sibling must not read as registered, got %q", line)
		}
		if !strings.Contains(line, sibling) {
			t.Errorf("the failed line must name the command the registration runs, got %q", line)
		}
		if !strings.Contains(line, claudeRegistrationRepair) {
			t.Errorf("the failed line must name the repair, got %q", line)
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
		if !strings.Contains(line, claudeRegistrationRepair) {
			t.Errorf("the failed line must name the repair, got %q", line)
		}
	})

	t.Run("a registration with no arguments is drift", func(t *testing.T) {
		statusEnv(t)
		ghostPath, claudeDir := claudeRegistrationFixture(t)
		// A registration that carries no Args line at all: the server would
		// start the CLI instead of the MCP server.
		writeRegistration(t, claudeDir,
			"ghost:\n  Scope: User\n  Type: stdio\n  Command: "+ghostPath+"\n")

		var out bytes.Buffer
		if _, err := Status(&out); err != nil {
			t.Fatalf("Status: %v", err)
		}
		line := statusLineContaining(out.String(), "MCP server")
		if !strings.Contains(line, "✗") {
			t.Fatalf("a registration with no arguments must not read as registered, got %q", line)
		}
		if !strings.Contains(line, "none") {
			t.Errorf("the failed line must name the arguments the registration carries, got %q", line)
		}
		if !strings.Contains(line, claudeRegistrationRepair) {
			t.Errorf("the failed line must name the repair, got %q", line)
		}
	})

	t.Run("a registration running an http entry is drift", func(t *testing.T) {
		statusEnv(t)
		_, claudeDir := claudeRegistrationFixture(t)
		// A registration that names a server but runs no command: the client
		// would speak HTTP to a server that is not ghost's.
		writeRegistration(t, claudeDir,
			"ghost:\n  Scope: User\n  Type: http\n  URL: https://example.com/mcp\n")

		var out bytes.Buffer
		if _, err := Status(&out); err != nil {
			t.Fatalf("Status: %v", err)
		}
		line := statusLineContaining(out.String(), "MCP server")
		if !strings.Contains(line, "✗") {
			t.Fatalf("a registration that runs no command must not read as registered, got %q", line)
		}
		if !strings.Contains(line, "does not run the ghost binary") {
			t.Errorf("the failed line must say the registration runs no ghost binary, got %q", line)
		}
		if !strings.Contains(line, claudeRegistrationRepair) {
			t.Errorf("the failed line must name the repair, got %q", line)
		}
	})

	t.Run("a registration running a symlink onto the ghost is registered", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("creating a symlink on Windows needs Developer Mode or elevated rights")
		}
		statusEnv(t)
		ghostPath, claudeDir := claudeRegistrationFixture(t)
		// The same binary under a second name: the identity is the file, so
		// this is the same registration rather than a different one.
		link := filepath.Join(t.TempDir(), "ghost-link")
		if err := os.Symlink(ghostPath, link); err != nil {
			t.Fatalf("link the ghost binary: %v", err)
		}
		writeRegistration(t, claudeDir, claudeRegistrationText(link, "mcp"))

		var out bytes.Buffer
		if _, err := Status(&out); err != nil {
			t.Fatalf("Status: %v", err)
		}
		line := statusLineContaining(out.String(), "MCP server")
		if !strings.Contains(line, "✓") {
			t.Fatalf("a registration running a symlink onto the resolved ghost must read as registered, got %q", line)
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
