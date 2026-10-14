package mcpinit

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// staleGhostCommand is the command path the fake claude reports as already
// registered under the name "ghost". It is a path this build is replacing (the
// stale-binary case), and it is the registration a failed add-json must put
// back. The token doubles as the marker the fake recognises in an add-json
// argument, so the new registration and the restore are told apart without the
// fake knowing anything about the caller's paths.
const staleGhostCommand = "/usr/local/bin/ghost-old/ghost"

// staleMarker is the substring both the POSIX and the Windows fake match on.
// It appears in staleGhostCommand and in the JSON that re-adds it.
const staleMarker = "ghost-old"

// addJSONRefusal is the failure the fake `claude mcp add-json` answers with, in
// the words the real CLI uses.
const addJSONRefusal = "Error: could not write the claude user config"

// removeJSONRefusal is the failure the fake `claude mcp remove` answers with.
const removeJSONRefusal = "Error: could not remove the claude MCP server"

// writeFakeClaude installs a fake `claude` that answers `mcp get ghost` with
// the registration in get.out, records every invocation's argv in calls, fails
// a remove while fail-remove exists, fails an add-json while fail-add exists, and
// fails one naming the stale command while fail-restore exists. The markers are
// files the test creates in the same directory, so the fake carries no knowledge
// of the test's expectations beyond them.
func writeFakeClaude(t *testing.T, dir string) string {
	t.Helper()
	name := "claude"
	var script string
	if runtime.GOOS == "windows" {
		name += ".bat"
		script = "@echo off\r\n" +
			">>\"" + dir + "\\calls\" echo %*\r\n" +
			"if \"%1\"==\"mcp\" if \"%2\"==\"get\" (\r\n" +
			"  type \"" + dir + "\\get.out\"\r\n" +
			"  exit /b 0\r\n" +
			")\r\n" +
			"if \"%1\"==\"mcp\" if \"%2\"==\"remove\" (\r\n" +
			"  if exist \"" + dir + "\\fail-remove\" (\r\n" +
			"    echo " + removeJSONRefusal + " 1>&2\r\n" +
			"    exit /b 1\r\n" +
			"  )\r\n" +
			"  >>\"" + dir + "\\removed\" echo ok\r\n" +
			"  exit /b 0\r\n" +
			")\r\n" +
			"set line=%*\r\n" +
			"if not \"%line:" + staleMarker + "=%\"==\"%line%\" (\r\n" +
			"  if exist \"" + dir + "\\fail-restore\" (\r\n" +
			"    echo " + addJSONRefusal + " 1>&2\r\n" +
			"    exit /b 1\r\n" +
			"  )\r\n" +
			"  >>\"" + dir + "\\restored\" echo %line%\r\n" +
			"  exit /b 0\r\n" +
			")\r\n" +
			"if exist \"" + dir + "\\fail-add\" (\r\n" +
			"  echo " + addJSONRefusal + " 1>&2\r\n" +
			"  exit /b 1\r\n" +
			")\r\n" +
			"exit /b 0\r\n"
	} else {
		script = "#!/bin/sh\n" +
			"printf '%s\\n' \"$*\" >> '" + dir + "/calls'\n" +
			"if [ \"$1\" = mcp ] && [ \"$2\" = get ]; then\n" +
			"  cat '" + dir + "/get.out'\n" +
			"  exit 0\n" +
			"fi\n" +
			"if [ \"$1\" = mcp ] && [ \"$2\" = remove ]; then\n" +
			"  if [ -f '" + dir + "/fail-remove' ]; then\n" +
			"    echo '" + removeJSONRefusal + "' >&2\n" +
			"    exit 1\n" +
			"  fi\n" +
			"  echo ok >> '" + dir + "/removed'\n" +
			"  exit 0\n" +
			"fi\n" +
			"case \"$*\" in\n" +
			"*" + staleMarker + "*)\n" +
			"  if [ -f '" + dir + "/fail-restore' ]; then\n" +
			"    echo '" + addJSONRefusal + "' >&2\n" +
			"    exit 1\n" +
			"  fi\n" +
			"  printf '%s\\n' \"$*\" >> '" + dir + "/restored'\n" +
			"  exit 0\n" +
			"  ;;\n" +
			"esac\n" +
			"if [ -f '" + dir + "/fail-add' ]; then\n" +
			"  echo '" + addJSONRefusal + "' >&2\n" +
			"  exit 1\n" +
			"fi\n" +
			"exit 0\n"
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write the fake claude: %v", err)
	}
	return path
}

// writeRegistrationText writes the `claude mcp get ghost` output for a stale
// ghost registration, which is the registration a rollback must restore.
func writeRegistrationText(t *testing.T, dir string) {
	t.Helper()
	command := staleGhostCommand
	if runtime.GOOS == "windows" {
		command = `C:\ghost-old\ghost`
	}
	text := "ghost:\n  Scope: User\n  Type: stdio\n  Command: " + command + "\n  Args: mcp\n"
	if err := os.WriteFile(filepath.Join(dir, "get.out"), []byte(text), 0o644); err != nil {
		t.Fatalf("write the registration text: %v", err)
	}
}

func marker(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
		t.Fatalf("write marker %s: %v", name, err)
	}
}

// TestRegisterMCPRestoresThePreviousRegistrationOnAddFailure is the #1216
// rollback: the registration `claude mcp remove` deleted before `add-json`
// failed is re-added, so a repair that could not land does not leave every
// Claude Code session with no ghost tools until a re-run succeeds.
func TestRegisterMCPRestoresThePreviousRegistrationOnAddFailure(t *testing.T) {
	dir := t.TempDir()
	claude := writeFakeClaude(t, dir)
	writeRegistrationText(t, dir)
	marker(t, dir, "fail-add")

	var out bytes.Buffer
	err := registerMCP(&out, filepath.Join(dir, "bin", "ghost"), claude, false)
	if err == nil {
		t.Fatal("a refused add-json must still fail the run")
	}

	restored, readErr := os.ReadFile(filepath.Join(dir, "restored"))
	if readErr != nil {
		t.Fatalf("the rollback must re-add the previous registration: %v", readErr)
	}
	if !strings.Contains(string(restored), staleMarker) {
		t.Errorf("the rollback must re-add the stale registration, got %q", restored)
	}
	if !strings.Contains(err.Error(), "restored") {
		t.Errorf("the error must say the previous registration was restored, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "claude mcp add-json") {
		t.Errorf("the error must still name the failed add-json, got %q", err.Error())
	}
}

// TestRegisterMCPNamesTheRegistrationRemovedWhenRestoreFails pins the floor
// the issue asks for: when the rollback cannot put the previous registration
// back, the error says the registration was removed, so the reader knows a
// re-run is not cosmetic.
func TestRegisterMCPNamesTheRegistrationRemovedWhenRestoreFails(t *testing.T) {
	dir := t.TempDir()
	claude := writeFakeClaude(t, dir)
	writeRegistrationText(t, dir)
	marker(t, dir, "fail-add")
	marker(t, dir, "fail-restore")

	var out bytes.Buffer
	err := registerMCP(&out, filepath.Join(dir, "bin", "ghost"), claude, false)
	if err == nil {
		t.Fatal("a refused add-json must still fail the run")
	}
	if !strings.Contains(err.Error(), "removed") {
		t.Errorf("the error must say the previous registration was removed, got %q", err.Error())
	}
	if strings.Contains(err.Error(), "registration was restored") {
		t.Errorf("a failed rollback must not claim a restore, got %q", err.Error())
	}
}

// TestRegisterMCPDoesNotRollBackARegistrationTheRemoveLeftInPlace pins the case
// the rollback must not touch: a `claude mcp remove` that failed did not take
// the previous registration out, so re-adding it would collide with itself and
// the error must say the registration most likely survived instead.
func TestRegisterMCPDoesNotRollBackARegistrationTheRemoveLeftInPlace(t *testing.T) {
	dir := t.TempDir()
	claude := writeFakeClaude(t, dir)
	writeRegistrationText(t, dir)
	marker(t, dir, "fail-add")
	marker(t, dir, "fail-remove")

	var out bytes.Buffer
	err := registerMCP(&out, filepath.Join(dir, "bin", "ghost"), claude, false)
	if err == nil {
		t.Fatal("a refused add-json must still fail the run")
	}
	if !strings.Contains(err.Error(), "could not be removed") {
		t.Errorf("the error must name the failed remove, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "still registered") {
		t.Errorf("the error must say the registration most likely survived, got %q", err.Error())
	}
	if _, readErr := os.ReadFile(filepath.Join(dir, "restored")); readErr == nil {
		t.Error("a registration the remove left in place must not be re-added")
	}
}

// TestRegisterMCPWithoutAPreviousRegistrationReportsNoRollback holds the other
// side: with nothing registered there is nothing to roll back, so the error is
// the one it always was and the fake sees a single add-json.
func TestRegisterMCPWithoutAPreviousRegistrationReportsNoRollback(t *testing.T) {
	dir := t.TempDir()
	claude := writeFakeClaude(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "get.out"), []byte("No MCP server found with name: ghost\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	marker(t, dir, "fail-add")

	var out bytes.Buffer
	err := registerMCP(&out, filepath.Join(dir, "bin", "ghost"), claude, false)
	if err == nil {
		t.Fatal("a refused add-json must still fail the run")
	}
	want := "claude mcp add-json: " + addJSONRefusal + ": exit status 1"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
	calls, readErr := os.ReadFile(filepath.Join(dir, "restored"))
	if readErr == nil {
		t.Errorf("nothing was registered, so there is nothing to restore, got %q", calls)
	}
}
