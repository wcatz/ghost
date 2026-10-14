package mcpinit

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// staleGhostCommand is the command path the previous registration reports and a
// rollback re-adds. It is a path this build is replacing (the stale-binary case),
// and it carries a token no path of this build's own install can hold — which is
// how a test tells a re-add of the PREVIOUS registration from a re-add of the
// one that just failed.
const staleGhostCommand = "/usr/local/bin/ghost-old/ghost"

// staleMarker is that token. The POSIX fake routes the two add-json calls on it;
// the Windows fake routes them by order and the assertion on what it recorded
// does the same job, because cmd refuses to parse a conditional whose expansion
// carries the JSON a re-add passes.
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
// argv of every add-json it accepts in adds and of every remove it accepts in
// removed. An add-json the fake refuses records nothing, so a case that must not
// roll back is pinned by the absence of either file.
//
// Both scripts are FLAT — no branch nested inside another — and neither expands
// the add-json argv into a conditional: cmd.exe refuses to parse an `if` whose
// expansion contains the JSON a re-add passes (quotes that unbalance the line,
// which is what made the first attempt's nested check silently never fire). The
// fake tells the two add-json calls apart by ORDER, and each test still pins
// WHICH config came back by what it recorded: `TestRegisterMCPRestoresThe-
// PreviousRegistrationOnAddFailure` is the one that catches a rollback that
// re-adds the new registration instead of the previous one.
func writeFakeClaude(t *testing.T, dir string, f claudeFake) string {
	t.Helper()
	name := "claude"
	var script string
	if runtime.GOOS == "windows" {
		name += ".bat"
		// FLAT, and NO argv ever lands inside a conditional. Two rules, both
		// learned by breaking CI: cmd refuses to parse an `if` whose expansion
		// contains the JSON's quotes (which is why the nested `if exist` of the
		// first attempt never fired), and only top-level `if exist` is reliable
		// there. The fake therefore tells the two add-json calls apart by order
		// — the first is the new registration to refuse, the second is the
		// rollback — and the tests still pin WHICH config came back by what the
		// fake recorded.
		script = "@echo off\r\n" +
			">>\"" + dir + "\\calls\" echo %*\r\n" +
			"if \"%1\"==\"mcp\" if \"%2\"==\"get\" goto do_get\r\n" +
			"if \"%1\"==\"mcp\" if \"%2\"==\"remove\" goto do_remove\r\n" +
			"if exist \"" + dir + "\\did-add\" goto do_restore\r\n" +
			">>\"" + dir + "\\did-add\" echo go\r\n" +
			failLine(f.addFails, addJSONRefusal) +
			">>\"" + dir + "\\adds\" echo %*\r\n" +
			"exit /b 0\r\n" +
			":do_get\r\n" +
			"type \"" + dir + "\\get.out\"\r\n" +
			"exit /b 0\r\n" +
			":do_remove\r\n" +
			failLine(f.removeFails, removeJSONRefusal) +
			">>\"" + dir + "\\removed\" echo ok\r\n" +
			"exit /b 0\r\n" +
			":do_restore\r\n" +
			failLine(f.restoreFails, addJSONRefusal) +
			">>\"" + dir + "\\adds\" echo %*\r\n" +
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
			"  ;;\n" +
			"*)\n" +
			failLine(f.addFails, addJSONRefusal) +
			"  ;;\n" +
			"esac\n" +
			"printf '%s\\n' \"$*\" >> '" + dir + "/adds'\n" +
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

	restored, readErr := os.ReadFile(filepath.Join(dir, "adds"))
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
	requireMissing(t, dir, "adds")
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
	requireMissing(t, dir, "adds")
}

// rollbackBase is the error a refused add-json raises, for the tests that call
// the rollback directly rather than through registerMCP.
func rollbackBase() error {
	return errors.New("claude mcp add-json: " + addJSONRefusal + ": exit status 1")
}

// lastRestoredAdd reads back the command line of the add-json the fake
// accepted, so a test can assert the re-add carries what it claims rather than
// only what the error text says about it. It is returned as the raw recorded
// text and asserted on by substring, NOT parsed as JSON: cmd.exe re-quotes and
// backslash-escapes the JSON a re-add passes, so the two platforms render the
// same argument differently and only the payload text survives both unchanged.
func lastRestoredAdd(t *testing.T, dir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "adds"))
	if err != nil {
		t.Fatalf("the rollback must re-add the previous registration: %v", err)
	}
	return string(raw)
}

// TestRollbackRegistrationNamesWhatTheReAddDidNotCarry is the #1216 honesty
// clause: the registration is rebuilt from `claude mcp get`'s rendering, so a
// bare environment block and an argument holding a space cannot both come
// across, and the error has to say so instead of claiming a restore that lost
// them. A reader who is told "restored" over a dropped variable re-runs nothing.
func TestRollbackRegistrationNamesWhatTheReAddDidNotCarry(t *testing.T) {
	dir := t.TempDir()
	claude := writeFakeClaude(t, dir, claudeFake{})
	previous := "ghost:\n  Scope: User\n  Type: stdio\n" +
		"  Command: " + staleGhostCommand + "\n" +
		"  Args: mcp --flag with space\n" +
		"  Environment: GHOST_TOKEN=abc123\n"
	writeRegistration(t, dir, previous)

	err := rollbackRegistration(claude, previous, true, nil, rollbackBase())
	if err == nil {
		t.Fatal("the rollback must still fail the run")
	}
	if !strings.Contains(err.Error(), "re-added without") {
		t.Errorf("the error must say the re-add was partial, got %q", err.Error())
	}
	for _, want := range []string{"its environment", "its arguments"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must name %s as not carried, got %q", want, err.Error())
		}
	}
	if strings.Contains(err.Error(), "registration was restored") {
		t.Errorf("a partial re-add must not claim a restore, got %q", err.Error())
	}
	// Best effort still lands: the command comes back even though the rest does not.
	recorded := lastRestoredAdd(t, dir)
	if !strings.Contains(recorded, staleGhostCommand) {
		t.Errorf("the re-add must carry the previous command, got %q", recorded)
	}
}

// TestRollbackRegistrationRestoresAnHTTPEntry is the http case: an http
// registration prints no Command line, so the old parser read it as "nothing
// registered" and reported the user's entry as unrestorable when the rollback
// could have put it back. A URL, headers in JSON and a spaced argument in a
// JSON array all come across, so the error can claim a restore.
func TestRollbackRegistrationRestoresAnHTTPEntry(t *testing.T) {
	dir := t.TempDir()
	claude := writeFakeClaude(t, dir, claudeFake{})
	previous := "ghost:\n  Scope: User\n  Type: http\n" +
		"  URL: https://example.com/mcp\n" +
		"  Headers: {\"Authorization\": \"Bearer token\"}\n"
	writeRegistration(t, dir, previous)

	err := rollbackRegistration(claude, previous, true, nil, rollbackBase())
	if err == nil {
		t.Fatal("the rollback must still fail the run")
	}
	if !strings.Contains(err.Error(), "the previous registration was restored") {
		t.Errorf("an http entry with its URL and headers carried must claim a restore, got %q", err.Error())
	}
	recorded := lastRestoredAdd(t, dir)
	for _, want := range []string{"https://example.com/mcp", "Bearer token"} {
		if !strings.Contains(recorded, want) {
			t.Errorf("the re-add must carry %q, got %q", want, recorded)
		}
	}
}

// TestRollbackRegistrationCarriesTheEnvironmentAndSpacedArgument rounds the pair
// out: when the rendering CAN carry them — an environment block as a JSON object
// and arguments as a JSON array — nothing is dropped and a spaced argument
// survives whole. Without it, "nothing was dropped" would only ever mean
// "nothing was printed".
func TestRollbackRegistrationCarriesTheEnvironmentAndSpacedArgument(t *testing.T) {
	dir := t.TempDir()
	claude := writeFakeClaude(t, dir, claudeFake{})
	previous := "ghost:\n  Scope: User\n  Type: stdio\n" +
		"  Command: " + staleGhostCommand + "\n" +
		"  Args: [\"mcp\", \"--flag with space\"]\n" +
		"  Environment: {\"GHOST_TOKEN\": \"abc123\"}\n"
	writeRegistration(t, dir, previous)

	err := rollbackRegistration(claude, previous, true, nil, rollbackBase())
	if err == nil {
		t.Fatal("the rollback must still fail the run")
	}
	if !strings.Contains(err.Error(), "the previous registration was restored") {
		t.Errorf("a re-add that carried everything must claim a restore, got %q", err.Error())
	}
	recorded := lastRestoredAdd(t, dir)
	// The argument carried WHOLE renders as `--flag with space`; a whitespace
	// split would render it instead as `--flag","with","space`, so this
	// substring is what tells the two apart. The environment is checked by its
	// name and value, which only appear when the object was carried.
	if !strings.Contains(recorded, "--flag with space") {
		t.Errorf("the re-add must carry the argument whole, got %q", recorded)
	}
	for _, want := range []string{"GHOST_TOKEN", "abc123"} {
		if !strings.Contains(recorded, want) {
			t.Errorf("the re-add must carry the environment, got %q", recorded)
		}
	}
}

// writeRegistration is the `claude mcp get ghost` output a rollback reads back.
func writeRegistration(t *testing.T, dir, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "get.out"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
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
	requireMissing(t, dir, "adds")
	requireMissing(t, dir, "removed")
}
