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

// claudeFake says which of the three commands the fake claude refuses. The
// choices are BAKED INTO the script rather than read from marker files at run
// time: a nested `if exist` inside a batch block does not fire on Windows while
// the same check at the top level does, so the earlier marker-file fake refused
// an add-json it was meant to let through and the rollback tests passed or
// failed for the wrong reason there.
type claudeFake struct {
	removeFails  bool // `mcp remove` exits non-zero
	addFails     bool // an add-json that does NOT name the previous command exits non-zero
	restoreFails bool // an add-json that DOES name the previous command exits non-zero
}

// writeFakeClaude installs a fake `claude` that answers `mcp get ghost` with
// the registration in get.out and answers remove/add-json per f, recording the
// argv of an add-json that names the stale command in restored and of a remove
// the fake accepted in removed. Both scripts are flat: no branch is nested
// inside another, because that is the shape the Windows one has to survive.
func writeFakeClaude(t *testing.T, dir string, f claudeFake) string {
	t.Helper()
	name := "claude"
	var script string
	if runtime.GOOS == "windows" {
		name += ".bat"
		script = "@echo off\r\n" +
			">>\"" + dir + "\\calls\" echo %*\r\n" +
			"if \"%1\"==\"mcp\" if \"%2\"==\"get\" goto do_get\r\n" +
			"if \"%1\"==\"mcp\" if \"%2\"==\"remove\" goto do_remove\r\n" +
			"set \"line=%*\"\r\n" +
			"if not \"%line:" + staleMarker + "=%\"==\"%line%\" goto do_restore\r\n" +
			"goto do_add\r\n" +
			":do_get\r\n" +
			"type \"" + dir + "\\get.out\"\r\n" +
			"exit /b 0\r\n" +
			":do_remove\r\n" +
			failLine(f.removeFails, removeJSONRefusal) +
			">>\"" + dir + "\\removed\" echo ok\r\n" +
			"exit /b 0\r\n" +
			":do_restore\r\n" +
			failLine(f.restoreFails, addJSONRefusal) +
			">>\"" + dir + "\\restored\" echo %line%\r\n" +
			"exit /b 0\r\n" +
			":do_add\r\n" +
			failLine(f.addFails, addJSONRefusal) +
			"exit /b 0\r\n"
	} else {
		script = "#!/bin/sh\n" +
			"printf '%s\\n' \"$*\" >> '" + dir + "/calls'\n" +
			"if [ \"$1\" = mcp ] && [ \"$2\" = get ]; then\n" +
			"  cat '" + dir + "/get.out'\n" +
			"  exit 0\n" +
			"fi\n" +
			"if [ \"$1\" = mcp ] && [ \"$2\" = remove ]; then\n" +
			failLine(f.removeFails, removeJSONRefusal) +
			"  echo ok >> '" + dir + "/removed'\n" +
			"  exit 0\n" +
			"fi\n" +
			"case \"$*\" in\n" +
			"*" + staleMarker + "*)\n" +
			failLine(f.restoreFails, addJSONRefusal) +
			"  printf '%s\\n' \"$*\" >> '" + dir + "/restored'\n" +
			"  exit 0\n" +
			"  ;;\n" +
			"esac\n" +
			failLine(f.addFails, addJSONRefusal) +
			"exit 0\n"
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write the fake claude: %v", err)
	}
	return path
}

// failLine renders the refusal the current script needs, or nothing when that
// command is accepted. It emits a WHOLE command (echo plus exit, or the POSIX
// pair) so the caller never has to nest or conditionally split a block around
// it — the Windows form uses `&` rather than parentheses for exactly that
// reason.
func failLine(fails bool, message string) string {
	if !fails {
		return ""
	}
	if runtime.GOOS == "windows" {
		return "echo " + message + " 1>&2 & exit /b 1\r\n"
	}
	return "  echo '" + message + "' >&2\n" +
		"  exit 1\n"
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

// requireMissing fails the test when the fake recorded the named action, which
// is how a case that must not roll back is pinned.
func requireMissing(t *testing.T, dir, name string) {
	t.Helper()
	if b, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
		t.Errorf("the fake should not have recorded %s, got %q", name, b)
	}
}

// TestRegisterMCPRestoresThePreviousRegistrationOnAddFailure is the #1216
// rollback: the registration `claude mcp remove` deleted before `add-json`
// failed is re-added, so a repair that could not land does not leave every
// Claude Code session with no ghost tools until a re-run succeeds.
func TestRegisterMCPRestoresThePreviousRegistrationOnAddFailure(t *testing.T) {
	dir := t.TempDir()
	claude := writeFakeClaude(t, dir, claudeFake{addFails: true})
	writeRegistrationText(t, dir)

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
	claude := writeFakeClaude(t, dir, claudeFake{addFails: true, restoreFails: true})
	writeRegistrationText(t, dir)

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
	requireMissing(t, dir, "restored")
}

// TestRegisterMCPDoesNotRollBackARegistrationTheRemoveLeftInPlace pins the case
// the rollback must not touch: a `claude mcp remove` that failed did not take
// the previous registration out, so re-adding it would collide with itself and
// the error must say the registration most likely survived instead.
func TestRegisterMCPDoesNotRollBackARegistrationTheRemoveLeftInPlace(t *testing.T) {
	dir := t.TempDir()
	claude := writeFakeClaude(t, dir, claudeFake{removeFails: true, addFails: true})
	writeRegistrationText(t, dir)

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
	requireMissing(t, dir, "restored")
}

// TestRegisterMCPWithoutAPreviousRegistrationReportsNoRollback holds the other
// side: with nothing registered there is nothing to roll back, so the error is
// the one it always was and the fake sees a single add-json.
func TestRegisterMCPWithoutAPreviousRegistrationReportsNoRollback(t *testing.T) {
	dir := t.TempDir()
	claude := writeFakeClaude(t, dir, claudeFake{addFails: true})
	if err := os.WriteFile(filepath.Join(dir, "get.out"), []byte("No MCP server found with name: ghost\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := registerMCP(&out, filepath.Join(dir, "bin", "ghost"), claude, false)
	if err == nil {
		t.Fatal("a refused add-json must still fail the run")
	}
	want := "claude mcp add-json: " + addJSONRefusal + ": exit status 1"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
	requireMissing(t, dir, "restored")
	requireMissing(t, dir, "removed")
}
