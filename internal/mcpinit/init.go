package mcpinit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/claudeimport"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/scratch"
)

// Run executes the 10-step Claude Code integration setup.
// When dryRun is true, it reports what would change without modifying anything.
func Run(w io.Writer, dryRun bool) error {
	// A Claude Code plugin install owns the MCP registration, hooks, and
	// permissions declaratively, so init must defer rather than double-wire
	// and fight it. PluginInstalled (not RunningAsPlugin) also catches the
	// case where the user runs the standalone binary while a plugin is
	// installed.
	if PluginInstalled() {
		_, _ = fmt.Fprintln(w, "The ghost Claude Code plugin manages this integration — skipping `ghost mcp init` wiring (use `/plugin update` in Claude Code to update it).")
		_, _ = fmt.Fprintln(w, "To switch to init-managed wiring, uninstall the plugin first.")
		return nil
	}

	if dryRun {
		_, _ = fmt.Fprintf(w, "\nDry run — showing what would change:\n\n")
	}

	// Validate settings.json before any step touches it. Steps 5-8 read the
	// document, permissions, permissions.allow, hooks and each event, so a
	// structurally invalid file would otherwise be discovered only after step 4
	// had registered the MCP server — leaving Claude Code with a ghost server
	// but no hooks, no tool permissions and autoMemoryEnabled untouched, or
	// panicking on the nil map a JSON null decodes into. Refusing here registers
	// nothing and leaves the file exactly as the user wrote it. No retryHint:
	// the file stays invalid until the named field is fixed, so a re-run hint
	// would misdirect exactly the way the Step 1 comment below describes.
	if err := validateSettingsFile(); err != nil {
		return err
	}

	// A refusal from the data-directory resolution stops init outright (#1321).
	// Step 2's scratch root and step 9's import both resolve the directory
	// through the same guard and both downgrade a failure there to a
	// "(continuing)" warning, so an install whose data directory was refused
	// still registered the client, wrote settings, printed "Done!" and exited
	// 0 — a script branching on the exit code could not tell the install's
	// data-directory step had been refused. Resolving the directory once, here,
	// makes the refusal the run's outcome and leaves nothing half-wired, and it
	// is before the steps so no other failure is promoted: every OTHER scratch
	// or per-project failure keeps its best-effort warning, and only the
	// guard's refusal is fatal. config.DataDirPath is a pure query that creates
	// nothing (#721) — asking it here cannot leave the phantom directory the
	// guard exists to prevent, and DataDirPath is the one place the guard runs,
	// so no refusal can reach this run another way.
	if _, err := config.DataDirPath(); err != nil {
		return fmt.Errorf("data directory: %w", err)
	}

	// Step 1: Prerequisites. No retryHint here: a missing binary is an
	// install-first problem that re-running init cannot fix, and the
	// claude-missing error carries its own next-step guidance (alternative
	// clients) that the generic re-run hint would bury.
	_, _ = fmt.Fprintln(w, "[1/10] Checking prerequisites...")
	ghostBin, claudeBin, err := checkPrereqs(w, "claude")
	if err != nil {
		return err
	}

	// Step 2: Scratch space.
	_, _ = fmt.Fprintln(w, "\n[2/10] Preparing scratch space...")
	ensureScratchSpace(w, dryRun)

	// Step 3: Config file.
	_, _ = fmt.Fprintln(w, "\n[3/10] Ensuring config file...")
	if err := ensureConfigBootstrap(w, dryRun); err != nil {
		return retryHint(err)
	}

	// Step 4: MCP server registration.
	_, _ = fmt.Fprintln(w, "\n[4/10] Registering MCP server...")
	if err := registerMCP(w, ghostBin, claudeBin, dryRun); err != nil {
		return retryHint(err)
	}

	// Step 5: Tool permissions.
	_, _ = fmt.Fprintln(w, "\n[5/10] Adding tool permissions...")
	settingsFile, err := ensurePermissions(w, dryRun)
	if err != nil {
		return retryHint(err)
	}

	// Step 6: SessionStart hook.
	_, _ = fmt.Fprintln(w, "\n[6/10] Configuring SessionStart hook...")
	if err := ensureHook(w, settingsFile, ghostBin, dryRun); err != nil {
		return retryHint(err)
	}

	// Step 7: Stop hook.
	_, _ = fmt.Fprintln(w, "\n[7/10] Configuring Stop and working-moment hooks...")
	if err := ensureStopHook(w, settingsFile, ghostBin, dryRun); err != nil {
		return retryHint(err)
	}
	if err := ensureWorkingMomentHooks(w, settingsFile, ghostBin, dryRun); err != nil {
		return retryHint(err)
	}

	// Step 8: Disable Claude Code's built-in file memory.
	_, _ = fmt.Fprintln(w, "\n[8/10] Disabling Claude Code built-in memory...")
	if err := ensureAutoMemoryDisabled(w, settingsFile, dryRun); err != nil {
		return retryHint(err)
	}

	// Save settings (steps 5-8 all modify it).
	if dryRun {
		_, _ = fmt.Fprintln(w, "\n  (skipping settings write — dry run)")
	} else {
		if err := settingsFile.save(); err != nil {
			return retryHint(fmt.Errorf("save settings: %w", err))
		}
	}

	// Step 9: Import Claude Code memories.
	_, _ = fmt.Fprintln(w, "\n[9/10] Importing Claude Code memories...")
	projects, err := importMemories(w, dryRun)
	if err != nil {
		_, _ = fmt.Fprintf(w, "  ! import error: %v (continuing)\n", err)
	}

	// Step 10: Project memory redirects.
	_, _ = fmt.Fprintln(w, "\n[10/10] Writing project memory redirects...")
	writeRedirects(w, projects, dryRun)

	if dryRun {
		_, _ = fmt.Fprintln(w, "\nNo changes made (dry run).")
	} else {
		_, _ = fmt.Fprintln(w, "\nDone! Restart Claude Code to activate.")
	}
	return nil
}

// ensureConfigBootstrap creates the platform's user config file from the
// embedded example if it's missing.
// In dry-run mode it reports what would happen without creating anything.
func ensureConfigBootstrap(w io.Writer, dryRun bool) error {
	if dryRun {
		path, err := config.ConfigFilePath()
		if err != nil {
			return fmt.Errorf("config file path: %w", err)
		}
		if _, err := os.Stat(path); err == nil {
			_, _ = fmt.Fprintf(w, "  ✓ config file exists: %s\n", path)
		} else {
			_, _ = fmt.Fprintf(w, "  ~ would create config file: %s\n", path)
		}
		return nil
	}

	path, created, err := config.EnsureConfigFile()
	if err != nil {
		return fmt.Errorf("ensure config file: %w", err)
	}
	if created {
		_, _ = fmt.Fprintf(w, "  + created config file: %s\n", path)
	} else {
		_, _ = fmt.Fprintf(w, "  ✓ config file exists: %s\n", path)
	}
	return nil
}

// retryHint wraps an error with a re-run suggestion.
func retryHint(err error) error {
	return fmt.Errorf("%w\n  Re-run `ghost mcp init` to retry", err)
}

// ensureScratchSpace makes sure Ghost's owned scratch root (internal/scratch)
// exists and reaps per-invocation directories left behind by crashed harness
// children, so the root cannot grow without bound between lifecycles. Scratch
// upkeep is best-effort: a failure is reported and init continues, because the
// harnesses themselves fall back to the inherited temp dir when scratch is
// unavailable, and init failing over cleanup would be worse than the leak.
func ensureScratchSpace(w io.Writer, dryRun bool) {
	if dryRun {
		_, _ = fmt.Fprintln(w, "  ~ would prepare the scratch root and reap stale dirs")
		return
	}
	root, err := scratch.Root()
	if err != nil {
		_, _ = fmt.Fprintf(w, "  ! scratch root unavailable: %v (continuing)\n", err)
		return
	}
	removed, err := scratch.Reap(24 * time.Hour)
	if err != nil {
		_, _ = fmt.Fprintf(w, "  ! scratch root %s not reaped: %v (continuing)\n", root, err)
		return
	}
	_, _ = fmt.Fprintf(w, "  ✓ scratch root %s (reaped %d stale dirs)\n", root, removed)
}

// checkPrereqs verifies the binaries required for the given client target.
// The "claude" target requires both ghost and claude; the "opencode" target
// requires only ghost.
func checkPrereqs(w io.Writer, client string) (ghostBin, claudeBin string, err error) {
	ghostBin = findGhostBinary()
	if ghostBin == "" {
		return "", "", fmt.Errorf("ghost binary not found in PATH — install it first")
	}
	_, _ = fmt.Fprintf(w, "  ✓ ghost binary at %s\n", ghostBin)

	if client == "claude" {
		claudeBin = findBinary("claude")
		if claudeBin == "" {
			return "", "", fmt.Errorf("claude CLI not found in PATH — install Claude Code first\n" +
				"  Or configure a different MCP client: ghost mcp init --client opencode|codex|goose|all")
		}
		_, _ = fmt.Fprintf(w, "  ✓ claude CLI at %s\n", claudeBin)
	}

	return ghostBin, claudeBin, nil
}

// systemBinDirs are absolute install dirs probed after the home-relative ones.
// Tests override this to stay isolated from host binaries.
var systemBinDirs = []string{"/opt/homebrew/bin", "/usr/local/bin"}

// windowsBinExts is the deliberately pinned extension set findBinary's
// common-dir fallback probes on Windows: .exe first, then .cmd, then .bat —
// fixed order and fixed content on every machine. It is not PATHEXT: the
// machine's PATHEXT env var is user-customizable and covers more types
// (.com, .ps1, ...), so reading it would make the fallback's probe order
// and coverage vary per host. Accepted asymmetry: a bare name.ps1 in a
// probed dir would not be found by the fallback — deliberate.
var windowsBinExts = []string{".exe", ".cmd", ".bat"}

// commonBinCandidates returns the file names findBinary probes inside a common
// install dir: the bare name on every platform, plus the Windows executable
// extensions. The home-relative list covers claude's native installer target
// ~/.local/bin — %USERPROFILE%\.local\bin holding claude.exe on Windows —
// which is exactly the install a PATH-only lookup misses.
func commonBinCandidates(name string) []string {
	candidates := []string{name}
	if runtime.GOOS == "windows" {
		for _, ext := range windowsBinExts {
			candidates = append(candidates, name+ext)
		}
	}
	return candidates
}

// commonDirExecutable reports whether a probed fallback candidate may be
// returned. POSIX keeps the exec-bit gate. On Windows executability is
// conferred by the file extension, not mode bits: os.Stat reports regular
// files as 0666/0444 there (0111 appears only on directories), so the
// exec-bit test would reject every candidate — an extensionless file cannot
// run at all, while a .exe/.cmd/.bat file is executable by existence.
func commonDirExecutable(p string, st os.FileInfo) bool {
	if st.IsDir() {
		return false
	}
	if runtime.GOOS != "windows" {
		return st.Mode().Perm()&0o111 != 0
	}
	ext := strings.ToLower(filepath.Ext(p))
	for _, e := range windowsBinExts {
		if ext == e {
			return true
		}
	}
	return false
}

// findBinary locates name on PATH first, then falls back to common install
// directories that are typically not on PATH (e.g. ~/.local/bin for the claude
// native installer, ~/go/bin for go install). Home-relative dirs follow the
// effective HOME; the systemBinDirs list is absolute, so tests that must stay
// isolated from host binaries override it. On Windows the fallback also
// probes name.exe/.cmd/.bat, since executability there is by extension.
// Probing the pinned candidates directly instead of calling
// exec.LookPath(filepath.Join(dir, name)) is deliberate: LookPath would
// defer to the machine's PATHEXT order and content (env-dependent), while
// the pinned list keeps probe behavior deterministic across machines.
func findBinary(name string) string {
	if c := binaryCandidates(name); len(c) > 0 {
		return c[0]
	}
	return ""
}

// binaryCandidates lists every location findBinary would accept for name, in
// findBinary's order: the PATH hit first, then each executable common-dir hit.
func binaryCandidates(name string) []string {
	var out []string
	if p, err := exec.LookPath(name); err == nil {
		out = append(out, p)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return out
	}
	for _, dir := range append([]string{
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, "go", "bin"),
	}, systemBinDirs...) {
		for _, candidate := range commonBinCandidates(name) {
			p := filepath.Join(dir, candidate)
			if st, err := os.Stat(p); err == nil && commonDirExecutable(p, st) && !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
	}
	return out
}

// ghostProbeTimeout bounds each `ghost --version` probe in findGhostBinary.
const ghostProbeTimeout = 5 * time.Second

// findGhostBinary is findBinary("ghost") for paths written into client config
// and hooks, which run later with the user's normal environment. It returns
// the first candidate whose `--version` exits 0 — `--version` returns before
// any config or data-directory work — so a PATH entry that refuses to run
// here, such as a sandbox guard shim, is skipped for a working install (#1569).
// When no candidate runs it fails open to findBinary's answer.
func findGhostBinary() string {
	candidates := binaryCandidates("ghost")
	for _, p := range candidates {
		ctx, cancel := context.WithTimeout(context.Background(), ghostProbeTimeout)
		err := exec.CommandContext(ctx, p, "--version").Run()
		cancel()
		if err == nil {
			return p
		}
	}
	if len(candidates) > 0 {
		return candidates[0]
	}
	return ""
}

// registerMCP ensures the ghost MCP server is registered with claude.
func registerMCP(w io.Writer, ghostBin, claudeBin string, dryRun bool) error {
	// Check current registration.
	out, err := exec.Command(claudeBin, "mcp", "get", "ghost").CombinedOutput()
	currentOutput := string(out)

	// removeErr is nil unless the stale registration's removal failed, and is
	// read only by the rollback the add-json failure path runs.
	var removeErr error
	alreadyRegistered := err == nil && strings.Contains(currentOutput, "Command:")
	correctPath := strings.Contains(currentOutput, ghostBin)

	if alreadyRegistered && correctPath {
		_, _ = fmt.Fprintln(w, "  ✓ ghost MCP server already registered")
		return nil
	}

	if dryRun {
		if alreadyRegistered {
			_, _ = fmt.Fprintf(w, "  ~ would update ghost MCP server (command: %s)\n", ghostBin)
		} else {
			_, _ = fmt.Fprintf(w, "  ~ would register ghost MCP server (command: %s)\n", ghostBin)
		}
		return nil
	}

	// Remove stale registration before re-adding. Its result is kept: the remove
	// is the one thing that takes the previous registration out, so whether it
	// landed is what the rollback below has to know.
	if alreadyRegistered {
		if out, rerr := exec.Command(claudeBin, "mcp", "remove", "-s", "user", "ghost").CombinedOutput(); rerr != nil {
			removeErr = fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), rerr)
		}
	}

	// Register or update.
	mcpConfig := map[string]any{
		"type":    "stdio",
		"command": ghostBin,
		"args":    []string{"mcp"},
	}
	configJSON, err := json.Marshal(mcpConfig)
	if err != nil {
		return fmt.Errorf("marshal mcp config: %w", err)
	}

	cmd := exec.Command(claudeBin, "mcp", "add-json", "-s", "user", "ghost", string(configJSON))
	if out, err := cmd.CombinedOutput(); err != nil {
		// A successful remove has already taken the previous registration out,
		// so a failed add leaves the user with no ghost server at all until a
		// re-run lands: put the old one back, or say that it is gone.
		return rollbackRegistration(claudeBin, currentOutput, alreadyRegistered, removeErr,
			fmt.Errorf("claude mcp add-json: %s: %w", strings.TrimSpace(string(out)), err))
	}

	if alreadyRegistered {
		_, _ = fmt.Fprintf(w, "  ✓ updated ghost MCP server (command: %s)\n", ghostBin)
	} else {
		_, _ = fmt.Fprintf(w, "  + registered ghost MCP server (command: %s)\n", ghostBin)
	}
	return nil
}

// rollbackRegistration answers a failed `claude mcp add-json` with the one fact
// the reader needs: what became of the registration Ghost deleted to make room
// for the new one. A successful remove takes the previous entry out before the
// add runs, so a refused add leaves every Claude Code session starting with no
// ghost tools until a re-run succeeds, and a repair that ends in that state must
// not read as a no-op. The entry is re-added from the `claude mcp get` output
// the same run already read. Nothing is rolled back in four cases, and each says
// which one it is rather than guessing: nothing was registered, where the error
// is the one it always was; a remove that failed, because the previous
// registration is then most likely still in place and re-adding it would
// collide; a registration the get output cannot describe; and — reported, never
// retried — a restore the client refuses, because a second attempt against the
// refused write is the failure that just happened.
//
// The get output is a human rendering and not the JSON add-json takes, so a
// re-add built from it is a reconstruction: it claims a RESTORE only when every
// line the output carried came across, and when something could not it says what
// (an environment block, a URL, headers, arguments whose boundaries the
// rendering cannot fix) and that the user should re-run or re-add it. A restore
// that silently lost a variable reads as clean and is not.
func rollbackRegistration(claudeBin, previousOutput string, hadPrevious bool, removeErr, base error) error {
	switch {
	case !hadPrevious:
		// Nothing was taken out, so there is nothing to put back and the
		// failure is the one it always was.
		return base
	case removeErr != nil:
		// The remove is the only thing that takes the entry out, and it did not
		// land: the registration is most likely still in place, and re-adding it
		// would collide with itself.
		return fmt.Errorf("%w (the previous registration could not be removed: %v — it is most likely still registered)", base, removeErr)
	}
	previous, ok := parseClaudeRegistration(previousOutput)
	if !ok {
		return fmt.Errorf("%w (the previous registration was removed and could not be restored)", base)
	}
	configJSON, err := json.Marshal(previous.config)
	if err != nil {
		return fmt.Errorf("%w (the previous registration was removed and could not be restored)", base)
	}
	if out, err := exec.Command(claudeBin, "mcp", "add-json", "-s", "user", "ghost", string(configJSON)).CombinedOutput(); err != nil {
		return fmt.Errorf("%w (the previous registration was removed and could not be restored: %s)",
			base, strings.TrimSpace(string(out)))
	}
	if len(previous.dropped) > 0 {
		return fmt.Errorf("%w (the previous registration was re-added without %s — re-run ghost mcp init or re-add them)",
			base, joinNames(previous.dropped))
	}
	return fmt.Errorf("%w (the previous registration was restored)", base)
}

// joinNames renders a list as prose: "a", "a and b", "a, b and c".
func joinNames(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// parseClaudeRegistration reads the registration `claude mcp get` printed, into
// the shape the add-json form takes:
//
//	ghost:
//	  Scope: User
//	  Type: stdio
//	  Command: /path/to/ghost
//	  Args: mcp
//	  Environment: {"GHOST_TOKEN": "..."}
//	  URL: https://example.com/mcp
//	  Headers: {"Authorization": "Bearer ..."}
//
// The get output is a rendering and not the JSON add-json takes, so a line is
// carried only where it can be carried WITHOUT guessing: a JSON array is the args
// verbatim (so an argument holding a space survives), a JSON object is the
// environment, the url or the headers verbatim, and a bare single token is that
// one argument. Several bare tokens are split on whitespace AND named as
// dropped, because one argument holding a space and two arguments print
// identically there. Scope is the registration's location rather than server
// config and is ignored; any other field the output carries is named as dropped.
// dropped is the list of those, so a message can name what a re-add left behind.
//
// The registration is restorable when it describes a command OR a url: an http
// entry prints no Command line, and reading that as "nothing registered" sent a
// user whose entry had been removed away from the rollback that would have put
// it back.
func parseClaudeRegistration(out string) (claudeRegistration, bool) {
	var previous claudeRegistration
	previous.config = map[string]any{}
	var command, args string
	var argsSet bool
	for _, line := range strings.Split(out, "\n") {
		field, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		field = strings.ToLower(strings.TrimSpace(field))
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		switch field {
		case "scope":
			// Where the registration lives, not what it runs.
		case "type":
			previous.config["type"] = value
		case "command":
			command = value
		case "url":
			previous.config["url"] = value
		case "args":
			args, argsSet = value, true
		case "env", "environment":
			if obj, ok := parseJSONObject(value); ok {
				previous.config["env"] = obj
			} else {
				previous.dropped = append(previous.dropped, "its environment")
			}
		case "headers":
			if obj, ok := parseJSONObject(value); ok {
				previous.config["headers"] = obj
			} else {
				previous.dropped = append(previous.dropped, "its headers")
			}
		default:
			previous.dropped = append(previous.dropped, "its "+field+" setting")
		}
	}
	if command != "" {
		previous.config["command"] = command
	}
	if argsSet {
		// A split the rendering cannot justify is still the best guess, so the
		// args are carried either way and only the CLAIM is qualified.
		parsed, exact := parseArgsLine(args)
		if len(parsed) > 0 {
			previous.config["args"] = parsed
		}
		if !exact {
			previous.dropped = append(previous.dropped, "its arguments")
		}
	}
	_, hasCommand := previous.config["command"]
	_, hasURL := previous.config["url"]
	return previous, hasCommand || hasURL
}

// claudeRegistration is one registration read back off `claude mcp get`, in the
// shape add-json takes, with the parts of it the re-add does not carry.
type claudeRegistration struct {
	config  map[string]any
	dropped []string
}

// parseArgsLine reads the rendered `Args:` line. A JSON array is carried
// verbatim and a single bare token is that one argument, both exact; several
// bare tokens are split on whitespace and reported as not exact, because the
// rendering cannot say whether it was one argument holding a space or two.
func parseArgsLine(value string) ([]string, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil, true
	}
	if strings.HasPrefix(trimmed, "[") {
		var args []string
		if err := json.Unmarshal([]byte(trimmed), &args); err != nil {
			return nil, false
		}
		return args, true
	}
	if !strings.ContainsAny(trimmed, " \t") {
		return []string{trimmed}, true
	}
	return strings.Fields(trimmed), false
}

// parseJSONObject reads a rendered field that carries a JSON object, reporting
// false when the rendering is not one — which is what names the field as
// dropped rather than inventing a value for it.
func parseJSONObject(value string) (map[string]any, bool) {
	var obj map[string]any
	if err := json.Unmarshal([]byte(value), &obj); err != nil || obj == nil {
		return nil, false
	}
	return obj, true
}

// validateSettingsFile loads ~/.claude/settings.json and validates every part
// of it `ghost mcp init` parses, before any step acts on the file. An absent
// file is valid: init populates it. A refusal names the file and the field and
// carries the edit that clears it rather than Run's generic re-run hint, which
// a re-run cannot satisfy — the file stays invalid until the user repairs it.
func validateSettingsFile() error {
	path, err := settingsPath()
	if err != nil {
		return err
	}
	sf, err := loadSettings(path)
	if err == nil {
		err = sf.validate()
	}
	if err != nil {
		return fmt.Errorf("%w\n  Fix the named field in %s, then re-run `ghost mcp init`", err, path)
	}
	return nil
}

// ensurePermissions loads settings.json and adds missing ghost permissions.
// On a dry run it reports the permissions it would add instead of the marker a
// written change carries.
func ensurePermissions(w io.Writer, dryRun bool) (*settingsFile, error) {
	path, err := settingsPath()
	if err != nil {
		return nil, err
	}

	sf, err := loadSettings(path)
	if err != nil {
		return nil, err
	}

	added, err := sf.addPermissions(ghostPermissions)
	if err != nil {
		return nil, fmt.Errorf("add permissions: %w", err)
	}

	existing := len(ghostPermissions) - len(added)
	if existing > 0 {
		_, _ = fmt.Fprintf(w, "  ✓ %d permissions already present\n", existing)
	}
	for _, p := range added {
		if dryRun {
			_, _ = fmt.Fprintf(w, "  ~ would add %s\n", p)
		} else {
			_, _ = fmt.Fprintf(w, "  + %s\n", p)
		}
	}
	if len(added) == 0 {
		_, _ = fmt.Fprintf(w, "  ✓ all %d ghost permissions configured\n", len(ghostPermissions))
	}

	return sf, nil
}

// shellQuote quotes s for safe embedding in a hook command line, matching
// the shell Claude Code invokes hooks through: cmd.exe on Windows (which
// does not treat ' as a quote character), POSIX shells elsewhere.
func shellQuote(s string) string {
	if runtime.GOOS == "windows" {
		return shellQuoteWindows(s)
	}
	return shellQuotePOSIX(s)
}

// shellQuoteWindows quotes s for cmd.exe, which has no escape for embedded
// double quotes other than doubling them.
func shellQuoteWindows(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// shellQuotePOSIX quotes s for POSIX shells using single quotes.
func shellQuotePOSIX(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// hookReconcileAction describes what reconcileHook did, for caller logging.
type hookReconcileAction int

const (
	hookUnchanged hookReconcileAction = iota
	hookAdded
	hookMigrated
)

// reconcileHook adds the hook if absent, migrates it in place if it exactly
// matches legacyCmd — the pre-#251 Windows-broken single-quoted form — or if
// it invokes a ghost binary at a stale path (#273, e.g. after an upgrade
// moved the binary), and otherwise leaves it untouched, including hand-edited
// commands that merely differ from desiredCmd, since ghost mcp init must stay
// non-destructive on re-run. The legacy-quoting match is exact, not
// substring, so a hand-edited wrapper that happens to also mention cmdSubstr
// is never rewritten.
//
// "Leave a command alone" and "count it as ghost's hook" are different
// decisions, and only the first one may be made about a command ghost does not
// own (#1326): another tool's command that merely ends in the same argument
// tail used to be read as ghost's hook, so init reported the event as already
// configured and installed nothing — Ghost's own hook then never ran under a
// success line. A command that does not run ghost stays exactly as written and
// ghost's own entry is added beside it; one that runs ghost through a prefix —
// `env X=1 ghost …`, `cd d && ghost …` — is ghost's own hook and gets no second
// entry, because two would fire it twice. Only a command whose LEADING token is
// ghost's binary is ever rewritten, by the legacy-quoting or stale-path repair.
func reconcileHook(sf *settingsFile, event, cmdSubstr, desiredCmd, legacyCmd string, isWindows bool) (hookReconcileAction, error) {
	return reconcileHookMatcher(sf, event, "", cmdSubstr, desiredCmd, legacyCmd, isWindows)
}

// reconcileHookMatcher is reconcileHook for an event whose entry carries a
// matcher (PostToolUse is matched by tool name). The matcher is used only when
// the hook is ADDED; an existing hook keeps whatever matcher it has, so a
// hand-edited one is never rewritten.
func reconcileHookMatcher(sf *settingsFile, event, matcher, cmdSubstr, desiredCmd, legacyCmd string, isWindows bool) (hookReconcileAction, error) {
	cmds, err := sf.hookCommands(event)
	if err != nil {
		return hookUnchanged, fmt.Errorf("parse existing %s hooks: %w", event, err)
	}

	// ghost's own command for this event, wherever it sits in the list. The
	// first command containing the argument tail is another tool's as often as
	// it is ghost's, and which one sorts first used to decide what a
	// substring read found — so on the foreign-first shape the reconciled
	// command was never ghost's at all.
	existingCmd, exists := "", false
	for _, cmd := range cmds {
		if isGhostHookCommand(cmd, cmdSubstr, desiredCmd) {
			existingCmd, exists = cmd, true
			break
		}
	}

	// No ghost hook here: add one. This covers both an event with no command
	// carrying the tail and one whose only such command belongs to another
	// tool, which is left exactly as the user wrote it.
	if !exists {
		return addGhostHook(sf, event, matcher, desiredCmd)
	}

	if existingCmd == desiredCmd {
		return hookUnchanged, nil
	}

	if isWindows && legacyCmd != desiredCmd {
		isLegacy, err := sf.hasExactHookCommand(event, legacyCmd)
		if err != nil {
			return hookUnchanged, fmt.Errorf("parse existing %s hooks: %w", event, err)
		}
		if isLegacy {
			if _, err := sf.replaceHookCommand(event, legacyCmd, desiredCmd); err != nil {
				return hookUnchanged, fmt.Errorf("migrate hook: %w", err)
			}
			return hookMigrated, nil
		}
	}

	if isStaleGhostHookCommand(existingCmd, desiredCmd) {
		if _, err := sf.replaceHookCommand(event, existingCmd, desiredCmd); err != nil {
			return hookUnchanged, fmt.Errorf("repair stale hook path: %w", err)
		}
		return hookMigrated, nil
	}

	return hookUnchanged, nil
}

// addGhostHook appends ghost's own hook entry for event. The matcher applies
// only to the entry being added: an existing entry keeps whatever matcher it
// has, so a hand-edited one is never rewritten.
func addGhostHook(sf *settingsFile, event, matcher, desiredCmd string) (hookReconcileAction, error) {
	entry := hookEntry{
		Matcher: matcher,
		Hooks: []hookAction{
			{Type: "command", Command: desiredCmd},
		},
	}
	if err := sf.addHook(event, entry); err != nil {
		return hookUnchanged, fmt.Errorf("add hook: %w", err)
	}
	return hookAdded, nil
}

// isGhostHookCommand reports whether cmd runs ghost for the event cmdSubstr
// names, wherever ghost sits in the command as long as it is in command
// position. It is a thin read of ghostHookTail, the one ownership rule this
// file keeps — shared with the status reader, so both answer the same question
// about the same file — and its doc records which shapes that rule accepts and
// which it holds foreign (#1326, #1337, #1339, #1358).
func isGhostHookCommand(cmd, cmdSubstr, desiredCmd string) bool {
	eventToken := strings.TrimPrefix(cmdSubstr, "hook ")
	if eventToken == cmdSubstr {
		return false
	}
	desiredBin, _, _ := splitHookCommand(desiredCmd)
	_, ok := ghostHookTail(cmd, eventToken, desiredBin)
	return ok
}

// ghostHookTail returns the tokens following `hook <eventToken>` when cmd runs
// ghost for that event — a token that is a ghost binary immediately before the
// tail — and false otherwise. It is the one ownership rule, shared by the
// settings reconcile and the status reader, so both answer the same question
// about the same file; the tail is returned so a reader can go on to check a
// flag that follows it, as status does for --source.
//
// Reading every token, not only the leading one, is what makes a hand-edited
// wrapper ghost's own: `env X=1 /path/ghost hook session-start` and
// `cd d && ghost hook stop` do invoke ghost, so init leaves them byte-identical
// and adds nothing beside them — a second entry would fire ghost's hook twice.
// The walk accepts those wrappers because their ghost binary sits in COMMAND
// position behind a prefix `startsCommand` sees through; a ghost binary that is
// only an ARGUMENT of another command (`echo /opt/bin/ghost hook stop …`, which
// runs echo) is not ghost's and gets ghost's own entry added beside it (#1358).
// Another tool whose command merely contains the words `hook session-start`,
// with no ghost-binary token before them, is not ghost's either (#1326).
//
// Two more shapes run ghost and are read here too, because a second entry fires
// the hook twice. A quoted argument can carry the whole invocation —
// `sh -c "ghost hook stop …"`, `bash -lc "…"` — and a command whose LAST token's
// quoting never closes is still a valid ghost invocation up to that point. Both
// readings are bounded: the ghost binary must be in command position there too,
// and the interior is read to a fixed depth.
func ghostHookTail(cmd, eventToken, ghostBin string) ([]string, bool) {
	// Fail soft: a trailing token whose quoting never closes still leaves the
	// tokens before it readable, and those are the ones that carry the hook.
	tokens, _ := hookCommandTokens(cmd)
	if tail, ok := ghostTailOver(tokens, eventToken, ghostBin, 0); ok {
		return tail, true
	}
	return nil, false
}

// shellConnectives are the shell words that start a new command in the command
// list they precede. `;`, `&&`, `||`, `|`, `&` and `exec` all leave the next
// word as a command, so a ghost binary there is one that runs.
var shellConnectives = map[string]bool{
	"exec": true, ";": true, "&&": true, "||": true, "|": true, "&": true,
}

// transparentPrefixes name the commands that run the word after their own
// flags and assignments unchanged: a ghost binary behind one of them is still
// the command that runs (`sudo ghost hook stop …`, `env -i X=1 ghost …`, `nice
// …`, `command …`, `nohup …`, `time …`, `doas …`, `timeout 5 …`), so a second
// entry would fire the hook twice (#1339). A few of them take a plain slot of
// their own before the command they run (prefixPositionalSlots) or an option
// with a separate operand (prefixOptionOperands), which is what puts the
// command after `timeout 5 ghost …` rather than the duration.
// `env`'s `-i` flag is swallowed as well, but only directly after `env`
// itself, so another tool's `-i` stays an argument of that tool.
var transparentPrefixes = map[string]bool{
	"sudo": true, "nice": true, "command": true, "env": true,
	"nohup": true, "time": true, "doas": true, "stdbuf": true,
	"ionice": true, "chrt": true, "taskset": true, "timeout": true,
}

// prefixPositionalSlots names the transparent prefix commands that take one
// plain word of their own before the command they run: timeout's DURATION
// (`timeout 5 ghost hook stop …`, `timeout -k 5 10 ghost …`), taskset's CPU
// mask (`taskset 0x3 ghost …`) and chrt's priority (`chrt -f 10 ghost …`). A
// word in that slot belongs to the command and not to the invocation, so the
// walk steps over it rather than stopping there and reading the real command as
// a foreign one.
var prefixPositionalSlots = map[string]int{
	"timeout": 1, "taskset": 1, "chrt": 1,
}

// prefixOptionOperands lists the options of the prefix commands that consume a
// separate following word as their operand, in both their short and long
// spellings (`timeout -k 5 10 ghost …`, `sudo --user alice ghost …`,
// `env --unset FOO ghost …`, `stdbuf -o L ghost …`, `ionice -c 2 -n 4 ghost …`,
// `taskset -c 0,1 ghost …`, `doas -u user ghost …`). It is keyed by command
// rather than shared, because the same spelling is an operand for one of them
// and nothing at all for another: `env`'s and `sudo`'s `-i` and `sudo`'s `-e`
// take none, so a shared table would read `sudo -e ghost …`'s ghost as a flag
// operand and then treat the real command as somebody else's. An option written
// `--opt=value` is SELF-CONTAINED — it carries its own operand inside the token,
// so only the exact `--opt` spelling is listed here and isLikelyOption walks the
// `--opt=value` token as a single word.
var prefixOptionOperands = map[string]map[string]bool{
	"sudo": {
		"-u": true, "--user": true, "-g": true, "--group": true,
		"-U": true, "--other-user": true, "-r": true, "--role": true,
		"-p": true, "--prompt": true, "-C": true, "--close-from": true,
		"-c": true, "--login-class": true, "-t": true, "--type": true,
		"-h": true, "--host": true, "-R": true, "--chroot": true,
		"-D": true, "--chdir": true, "-T": true, "--command-timeout": true,
	},
	"env": {
		"-u": true, "--unset": true, "-C": true, "--chdir": true,
		"-S": true, "--split-string": true,
	},
	"nice":    {"-n": true, "--adjustment": true},
	"timeout": {"-k": true, "--kill-after": true, "-s": true, "--signal": true},
	"stdbuf": {
		"-i": true, "--input": true, "-o": true, "--output": true,
		"-e": true, "--error": true,
	},
	"ionice": {
		"-c": true, "--class": true, "-n": true, "--classdata": true,
		"-p": true, "--pid": true,
	},
	"taskset": {"-c": true, "--cpu-list": true, "-p": true, "--pid": true},
	"doas":    {"-u": true, "--user": true},
}

// prefixSlotWord reports whether tok is the plain word a prefix command takes
// for itself before the command it runs, judged by SHAPE and not by position:
// without the shape test any word would qualify as the slot and the command
// itself would be swallowed, so `taskset -c 0,1 echo ghost hook stop …` (echo
// runs) would credit ghost while `taskset 0x3 ghost …` must. timeout's slot is
// a DURATION, taskset's a CPU mask or list, and chrt's a priority; a command
// with no slot of its own has no word that qualifies, so a plain word there is
// the command and belongs to somebody else.
func prefixSlotWord(base, tok string) bool {
	switch base {
	case "timeout":
		return isDurationWord(tok)
	case "taskset":
		return isCPUMaskWord(tok)
	case "chrt":
		return isPriorityWord(tok)
	}
	return false
}

// isDurationWord reports whether tok is a timeout DURATION: decimal digits with
// an optional fraction and an optional unit suffix (`5`, `0.5`, `10s`, `1.5m`,
// `2h`, `1d`), which is the form timeout accepts before the command it runs.
func isDurationWord(tok string) bool {
	i, digits := 0, 0
	for i < len(tok) && tok[i] >= '0' && tok[i] <= '9' {
		i++
		digits++
	}
	if i < len(tok) && tok[i] == '.' {
		i++
		for i < len(tok) && tok[i] >= '0' && tok[i] <= '9' {
			i++
			digits++
		}
	}
	if digits == 0 {
		return false
	}
	if i == len(tok) {
		return true
	}
	return i == len(tok)-1 && strings.IndexByte("smhd", tok[i]) >= 0
}

// isCPUMaskWord reports whether tok is a taskset CPU mask, optionally
// `0x`-prefixed, or a list of them separated by `,` and ranged with `-`
// (`0x3`, `3f`, `0,1`, `0-3`, `0,2-4`). A separator must sit between two
// hexadecimal digits, so `echo` and `-c` are not masks.
func isCPUMaskWord(tok string) bool {
	s := tok
	if len(s) > 2 && (s[:2] == "0x" || s[:2] == "0X") {
		s = s[2:]
	}
	if s == "" {
		return false
	}
	digits := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case isHexDigit(c):
			digits++
		case c == ',' || c == '-':
			if i == 0 || i == len(s)-1 || !isHexDigit(s[i-1]) || !isHexDigit(s[i+1]) {
				return false
			}
		default:
			return false
		}
	}
	return digits > 0
}

// isPriorityWord reports whether tok is a chrt priority: a plain decimal number
// (`chrt -f 10 ghost …`).
func isPriorityWord(tok string) bool {
	if tok == "" {
		return false
	}
	for i := 0; i < len(tok); i++ {
		if tok[i] < '0' || tok[i] > '9' {
			return false
		}
	}
	return true
}

// isHexDigit reports whether c is one of the hexadecimal digits a taskset mask
// is written with.
func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// prefixRegionBefore reports whether the words between the nearest transparent
// prefix command to the left of idx and idx itself are that command's own — its
// options with their operands, its assignment words (`timeout 5 X=1 ghost …`)
// and at most prefixPositionalSlots plain words, each of which must have the
// SHAPE that command's slot takes (prefixSlotWord) — and returns the index the
// walk should continue from: the word left of that command. It carries the
// shapes whose own words sit between the command and the invocation it runs
// (`timeout -k 5 10 ghost …`), which the plain walk cannot classify, and refuses
// a plain word none of them owns: a command with no slot (`echo 5 ghost …`
// leaves the 5 to echo) or a word of the wrong shape (`taskset -c 0,1 echo
// ghost …` leaves ghost to echo), because the ghost binary is then that
// command's argument.
func prefixRegionBefore(tokens []string, idx int) (int, bool) {
	head, base := -1, ""
	for j := idx; j >= 0; j-- {
		if b := hookCommandBase(tokens[j]); transparentPrefixes[b] {
			head, base = j, b
			break
		}
	}
	if head < 0 {
		return 0, false
	}
	operands, slots, positional := prefixOptionOperands[base], prefixPositionalSlots[base], 0
	for j := head + 1; j <= idx; j++ {
		tok := tokens[j]
		switch {
		case isAssignmentWord(tok):
		case operands[tok]:
			// The option's operand must be one of the words between the
			// command and the invocation, never the invocation's own token:
			// `timeout -k ghost hook stop …` names ghost as the duration.
			if j+1 > idx {
				return 0, false
			}
			j++
		case isLikelyOption(tok):
		default:
			if !prefixSlotWord(base, tok) {
				return 0, false
			}
			positional++
		}
	}
	if positional > slots {
		return 0, false
	}
	return head - 1, true
}

// shellOptionTakesOperand is the same question for shell flags (`bash -o
// posix` → `posix`).
var shellOptionTakesOperand = map[string]bool{
	"-o": true, "--option": true, "-O": true,
}

// isLikelyOption reports whether a token is a command option: a single-dash
// (or long-dash) word, never a flagless command word. Used both for shells and
// for the transparent prefix commands.
func isLikelyOption(w string) bool {
	return len(w) > 1 && strings.HasPrefix(w, "-")
}

// shellInterpreters name the commands that run a later argument as a command
// list, and carriesCommandArgument reports whether the words before a token
// say the token is one: a shell's own command-list flag — a single-dash flag
// ending in `c`, which is how every one of them spells `-c` and combined
// forms like `-lc` — directly after a shell name (`sh -c SCRIPT`, `bash -lc
// SCRIPT`), or a shell name taking the script positionally (`busybox sh
// SCRIPT`). A flag belongs to any tool, so the flag form only counts after a
// shell: `othertool -c 'ghost hook stop …'` and `othertool --sync '…'` hand
// ghost's invocation over as DATA to another tool, and reading either as a
// command list is the false positive #1339 closed. Without this gate an
// ARGUMENT that merely mentions ghost — `othertool --note 'ghost hook stop …'`
// — would be read as a command that runs, which is the #1326 false positive.
var (
	shellInterpreters = map[string]bool{
		"sh": true, "bash": true, "zsh": true, "dash": true,
		"ksh": true, "ash": true, "busybox": true,
	}
)

func carriesCommandArgument(tokens []string, i int) bool {
	if i == 0 {
		return false
	}
	prev := tokens[i-1]
	if isShellCommandListFlag(prev) {
		return shellBehind(tokens, i)
	}
	return shellInterpreters[hookCommandBase(prev)]
}

// shellBehind reports whether a known shell name precedes its own command-list
// flag, even when the shell carries its own options before that flag
// (`sh -e -c "…"`, `bash -o posix -c "…"`, `bash --posix -c "…"`). Without
// this walk the options cancel the recognition and init appends a second
// entry beside a wrapper that already runs ghost (#1339 reviewer finding).
func shellBehind(tokens []string, i int) bool {
	idx := i - 2
	for idx >= 0 {
		tok := tokens[idx]
		if shellInterpreters[hookCommandBase(tok)] {
			return true
		}
		if isLikelyOption(tok) {
			idx--
			continue
		}
		// operand of an option-taking flag sitting immediately to the left
		if idx > 0 && shellOptionTakesOperand[tokens[idx-1]] {
			idx -= 2
			continue
		}
		return false
	}
	return false
}

// isShellCommandListFlag reports whether word is a single-dash flag ending in
// `c` — the shape every known shell spells `-c` and combined forms like `-lc`
// with. A long flag (`--sync`) never carries a command list, and the single-
// dash form only means one directly after a shell name
// (carriesCommandArgument).
func isShellCommandListFlag(word string) bool {
	return len(word) > 1 && strings.HasPrefix(word, "-") && !strings.HasPrefix(word, "--") && strings.HasSuffix(word, "c")
}

// ghostTailOver walks tokens for a ghost binary immediately followed by the
// event's tail. A ghost binary counts only in COMMAND position, at every depth:
// the top level applies the same startsCommand walk as the interior of a quoted
// command list, so a leading wrapper's prefix — an assignment (`X=1 ghost …`), a
// transparent prefix word (`sudo ghost …`) or a connective (`cd d && ghost …`)
// — still reads as ghost's own, while a ghost binary that is only an ARGUMENT of
// another command (`echo /opt/bin/ghost hook stop …`, which runs echo) does not
// (#1358). A quoted argument is read INSIDE only when the word before it makes
// it a command list (`sh -c "…"`, `bash -lc "…"`), because otherwise it is data
// to the command that received it and a ghost binary named in it is somebody
// else's text — the #1326 false positive. Recursion is bounded in depth so a
// nested quote cannot recurse forever.
func ghostTailOver(tokens []string, eventToken, ghostBin string, depth int) ([]string, bool) {
	for i := 0; i < len(tokens); i++ {
		if isGhostToken(tokens[i], ghostBin) {
			if !startsCommand(tokens, i) {
				continue
			}
			if i+2 < len(tokens) && tokens[i+1] == "hook" && tokens[i+2] == eventToken {
				return tokens[i+3:], true
			}
			continue
		}
		if depth < 2 && carriesCommandArgument(tokens, i) &&
			strings.ContainsRune(tokens[i], ' ') {
			if inner, ok := hookCommandTokens(tokens[i]); ok {
				if tail, ok := ghostTailOver(inner, eventToken, ghostBin, depth+1); ok {
					return tail, true
				}
			}
		}
	}
	return nil, false
}

// startsCommand reports whether the token at i starts a command in the command
// list it belongs to: the first token, one right after a connective, or one
// preceded only by words that are not the command itself — an assignment
// prefix (`X=1`) or a transparent prefix word (`sudo ghost …`, `env -i X=1
// ghost …`) — so the scan walks back until something else appears and `echo
// sudo ghost hook stop` still runs `echo` (#1339). A transparent prefix word is
// compared by basename (`/usr/bin/env ghost …` runs ghost just as `env ghost …`
// does), the way shellBehind already resolves shell names by hookCommandBase,
// so a path-qualified wrapper keeps its command position instead of reading as
// a foreign command that gains a second entry.
//
// The boundary test is a CONNECTIVE TOKEN, never a token that merely contains a
// `;`, `&` or `|`: hookCommandTokens has already cut an UNQUOTED operator into a
// token of its own, so one still inside a token is quoted text — `othertool
// --note "a;b"`, `'x|y'`, a URL with `&` — and does not open command position,
// or an argument-position ghost binary after it would be credited and the event
// left unwired (#1358 review). That is the opposite direction from
// codexTokenEndsCommand, which stays boundary-on-contains because there a
// boundary means "do not credit" and the safe direction is to append ghost's
// canonical rule.
//
// Three more shapes leave the following word as a command. A group opener (`(`
// or `{`) opens a command list, so the first word inside it is in command
// position (`(cd /d; ghost …)`, `{ ghost …; }`). A word a prefix command owns as
// its own operand or slot is stepped over (prefixRegionBefore, `timeout -k 5 10
// ghost …`). And a newline arrives as the `;` token hookCommandTokens
// normalises it to, so `cd /d` + newline + `ghost …` is a command list too.
func startsCommand(tokens []string, i int) bool {
	if followsOptionOperandSlot(tokens, i) {
		return false
	}
	idx := i - 1
	for idx >= 0 {
		tok := tokens[idx]
		if shellConnectives[tok] {
			return true
		}
		if tok == "(" || tok == "{" {
			// A group opener: the first word inside it is a command.
			idx--
			continue
		}
		if isAssignmentWord(tok) {
			idx--
			continue
		}
		if transparentPrefixes[hookCommandBase(tok)] {
			idx--
			continue
		}
		if tok == "-i" && idx > 0 && hookCommandBase(tokens[idx-1]) == "env" {
			idx--
			continue
		}
		if isLikelyOption(tok) {
			idx--
			continue
		}
		if run, ok := prefixRegionBefore(tokens, idx); ok {
			idx = run
			continue
		}
		return false
	}
	return true
}

// followsOptionOperandSlot reports whether the word at i is the separate
// operand of the option left of it, for the nearest transparent prefix command
// (`timeout -k ghost …`, `env -u ghost …`, `sudo -u ghost …`): the binary is
// then that option's value, not the command the prefix runs. Only the
// per-command table counts: the shared one lists `-p`, which `time -p ghost …`
// takes no operand for. `--opt=value` forms are one token and never match.
func followsOptionOperandSlot(tokens []string, i int) bool {
	if i < 1 {
		return false
	}
	opt := tokens[i-1]
	for j := i - 2; j >= 0; j-- {
		if shellConnectives[tokens[j]] {
			return false
		}
		if b := hookCommandBase(tokens[j]); transparentPrefixes[b] {
			return prefixOptionOperands[b][opt]
		}
	}
	return false
}

// isAssignmentWord reports whether word is a shell assignment prefix such as
// `X=1`, which sets an environment variable rather than naming a command.
func isAssignmentWord(word string) bool {
	eq := strings.IndexByte(word, '=')
	return eq > 0
}

// isGhostToken reports whether one token of a hook command names a ghost
// binary: a ghost-named one under any path and any quoting, or the exact binary
// this run writes under whatever name it carries — the isBakedGhostBinary
// recognition the Codex merge applies, so a renamed binary stays recognised.
func isGhostToken(token, desiredBin string) bool {
	if isGhostBinaryName(token) {
		return true
	}
	return desiredBin != "" && isBakedGhostBinary(token, desiredBin)
}

// hookCommandTokens splits a hook command into its tokens, reversing the
// Windows double-quoting and POSIX single-quoting shellQuote produces exactly
// as splitHookCommand does for each token: a token is quoted as its first byte
// says, or bare up to the next space, so a quoted argument comes back as ONE
// token with its interior spaces intact. When a token's quoting never closes
// the split stops there and the function reports false — but it still returns
// the tokens read before that point, which are real: a trailing unclosed quote
// (`ghost hook stop …; echo "oops`) must not cost the reader the valid ghost
// invocation at the front of the command. Callers use the tokens either way.
//
// An operator in a BARE token is the shell's, so it is returned as a token of
// its own (`splitUnquotedOperators`): `cd /d;ghost …` and `build|tee` separate
// their commands exactly as the spaced forms do, while a `;`, `&` or `|` inside
// a QUOTED token survives as the user's text (`--note "a;b"`, a URL with `&`)
// and never reads as a command boundary (#1358 review).
func hookCommandTokens(cmd string) ([]string, bool) {
	var tokens []string
	// A NEWLINE separates commands in shell exactly as `;` does, so it is
	// normalised to that connective before the split: a hand-edited
	// `cd /d` + newline + `ghost hook stop …` is a command list, and the
	// leading-newline trim below must not swallow the separator itself.
	cmd = strings.ReplaceAll(cmd, "\r\n", "\n")
	cmd = strings.ReplaceAll(cmd, "\n", " ; ")
	for {
		// Leading whitespace is indentation, not an empty leading token: a
		// hand-edited `   /path/ghost hook stop …` must tokenise as the ghost
		// invocation it is, so the command-position walk reaches its binary.
		cmd = strings.TrimLeft(cmd, " \t")
		if cmd == "" {
			return tokens, false
		}
		quoted := cmd[0] == '"' || cmd[0] == '\''
		token, rest, ok := splitHookCommand(cmd)
		if !ok {
			return tokens, false
		}
		if quoted {
			tokens = append(tokens, token)
		} else {
			tokens = append(tokens, splitUnquotedOperators(token)...)
		}
		if rest == "" {
			return tokens, true
		}
		cmd = rest
	}
}

// splitUnquotedOperators cuts the shell operators out of a bare token into
// tokens of their own, so the command-position walk sees them where the shell
// would: `d;` is `d` and `;`, `cd&&` is `cd` and `&&`, and a pipe written
// without space (`build|tee`) separates the same way. Only the caller decides
// whether a token was quoted; a `;`, `&` or `|` inside a QUOTED token is the
// user's text (`--note "a;b"`, a grep pattern `'x|y'`, a URL with `&`) and is
// left whole, because crediting it would read an argument-position ghost binary
// as ghost's own and leave the event unwired (#1358 review).
//
// A `&` glued to a `>` on either side belongs to a redirection (`2>&1`,
// `&>file`), not to a command boundary: `othertool 2>&1 ghost hook stop …`
// passes ghost to othertool, so that token is left whole too.
func splitUnquotedOperators(token string) []string {
	if !strings.ContainsAny(token, ";&|)") {
		return []string{token}
	}
	var (
		pieces []string
		word   strings.Builder
	)
	flush := func() {
		if word.Len() > 0 {
			pieces = append(pieces, word.String())
			word.Reset()
		}
	}
	for i := 0; i < len(token); i++ {
		c := token[i]
		if c != ';' && c != '&' && c != '|' {
			word.WriteByte(c)
			continue
		}
		if c == '&' && ((i > 0 && token[i-1] == '>') || (i+1 < len(token) && token[i+1] == '>')) {
			word.WriteByte(c)
			continue
		}
		flush()
		// A run of the same operator stays one token, so `&&` and `||` read as
		// the connectives they are.
		j := i
		for j+1 < len(token) && token[j+1] == c {
			j++
		}
		pieces = append(pieces, token[i:j+1])
		i = j
	}
	flush()
	var out []string
	for _, p := range pieces {
		out = append(out, cutGroupCloser(p)...)
	}
	return out
}

// cutGroupCloser cuts the unquoted `)` that closes a subshell off the end of a
// bare word into a token of its own, so `(cd /d; ghost hook stop --source
// claude-code)` ends its tail at the closer and the value reads as
// `claude-code`. Only a `)` with no `(` left in the word to pair with is a
// closer: `$(date)` keeps its own.
func cutGroupCloser(word string) []string {
	if len(word) < 2 || word[len(word)-1] != ')' {
		return []string{word}
	}
	open := strings.Count(word, "(")
	end := len(word)
	for end > 0 && word[end-1] == ')' && strings.Count(word[:end], ")") > open {
		end--
	}
	if end == len(word) || end == 0 {
		return []string{word}
	}
	out := []string{word[:end]}
	for k := end; k < len(word); k++ {
		out = append(out, ")")
	}
	return out
}

// isStaleGhostHookCommand reports whether existingCmd invokes a ghost binary
// (basename "ghost" under the launcher extensions, case-insensitive) at a
// different path than desiredCmd, with the same trailing arguments — the #273
// case where an upgrade moved the binary and left the hook pointing at the old
// location. Hand-edited wrapper commands (whose leading token isn't a ghost
// binary) are left untouched.
func isStaleGhostHookCommand(existingCmd, desiredCmd string) bool {
	existingBin, existingRest, ok := splitHookCommand(existingCmd)
	if !ok || !isGhostBinaryName(existingBin) {
		return false
	}
	desiredBin, desiredRest, ok := splitHookCommand(desiredCmd)
	if !ok || !isGhostBinaryName(desiredBin) {
		return false
	}
	return existingBin != desiredBin && existingRest == desiredRest
}

// isGhostBinaryName reports whether path's basename is "ghost" under any of
// the extensions a launcher gives it: the native ".exe", or the ".bat"/".cmd"
// batch shim an install may place on PATH instead. The comparison is
// case-insensitive, and a Windows path is recognised on any host (the
// basename split handles both separators).
func isGhostBinaryName(path string) bool {
	base := strings.ToLower(hookCommandBase(path))
	for _, ext := range []string{".exe", ".bat", ".cmd"} {
		base = strings.TrimSuffix(base, ext)
	}
	return base == "ghost"
}

// hookCommandBase extracts the final path segment, splitting on both '/' and
// '\\' since a hook command embedding a Windows path may be parsed on any
// host OS (filepath.Base only splits on the host separator).
func hookCommandBase(path string) string {
	i := strings.LastIndexAny(path, `/\`)
	return path[i+1:]
}

// splitHookCommand extracts the leading executable token from a hook command
// string and the remainder (trimmed of leading whitespace), reversing
// shellQuoteWindows/shellQuotePOSIX. It handles the three forms
// ghost mcp init can produce: Windows double-quoting with ""-doubling,
// POSIX single-quoting with '\”-escaping, and a bare unquoted path.
func splitHookCommand(cmd string) (bin, rest string, ok bool) {
	if cmd == "" {
		return "", "", false
	}
	switch cmd[0] {
	case '"':
		var b strings.Builder
		i := 1
		for i < len(cmd) {
			if cmd[i] == '"' {
				if i+1 < len(cmd) && cmd[i+1] == '"' {
					b.WriteByte('"')
					i += 2
					continue
				}
				return b.String(), strings.TrimSpace(cmd[i+1:]), true
			}
			b.WriteByte(cmd[i])
			i++
		}
		return "", "", false
	case '\'':
		var b strings.Builder
		i := 1
		for i < len(cmd) {
			if cmd[i] == '\'' {
				if strings.HasPrefix(cmd[i:], `'\''`) {
					b.WriteByte('\'')
					i += 4
					continue
				}
				return b.String(), strings.TrimSpace(cmd[i+1:]), true
			}
			b.WriteByte(cmd[i])
			i++
		}
		return "", "", false
	default:
		i := strings.IndexByte(cmd, ' ')
		if i < 0 {
			return cmd, "", true
		}
		return cmd[:i], strings.TrimSpace(cmd[i+1:]), true
	}
}

// ensureHook adds a SessionStart hook if not already present, migrates it off
// the pre-#251 quoting that's broken under cmd.exe, and migrates any
// pre-contract bare invocation (`… hook session-start`, no --source) onto the
// contract-v1 form. A dry run reports what it would do and writes nothing.
func ensureHook(w io.Writer, sf *settingsFile, ghostBin string, dryRun bool) error {
	hookCmd := shellQuote(ghostBin) + " hook session-start --source claude-code"
	legacyCmd := shellQuotePOSIX(ghostBin) + " hook session-start"
	warnPercentPath(w, ghostBin)

	contractMigrated, err := migratePreContractHook(sf, "SessionStart", "hook session-start", hookCmd)
	if err != nil {
		return err
	}

	action, err := reconcileHook(sf, "SessionStart", "hook session-start", hookCmd, legacyCmd, runtime.GOOS == "windows")
	if err != nil {
		return err
	}
	switch {
	case action == hookAdded:
		if dryRun {
			_, _ = fmt.Fprintf(w, "  ~ would add SessionStart hook: %s\n", hookCmd)
		} else {
			_, _ = fmt.Fprintf(w, "  + added SessionStart hook: %s\n", hookCmd)
		}
	case action == hookMigrated || contractMigrated:
		if dryRun {
			_, _ = fmt.Fprintf(w, "  ~ would migrate SessionStart hook to current invocation: %s\n", hookCmd)
		} else {
			_, _ = fmt.Fprintf(w, "  + migrated SessionStart hook to current invocation: %s\n", hookCmd)
		}
	default:
		_, _ = fmt.Fprintln(w, "  ✓ SessionStart hook already configured")
	}
	return nil
}

// ensureStopHook adds a Stop hook if not already present, migrates it off the
// pre-#251 quoting that's broken under cmd.exe, and migrates any pre-contract
// bare invocation (`… hook stop`, no --source) onto the contract-v1 form. A dry
// run reports what it would do and writes nothing.
func ensureStopHook(w io.Writer, sf *settingsFile, ghostBin string, dryRun bool) error {
	hookCmd := shellQuote(ghostBin) + " hook stop --source claude-code"
	legacyCmd := shellQuotePOSIX(ghostBin) + " hook stop"

	contractMigrated, err := migratePreContractHook(sf, "Stop", "hook stop", hookCmd)
	if err != nil {
		return err
	}

	action, err := reconcileHook(sf, "Stop", "hook stop", hookCmd, legacyCmd, runtime.GOOS == "windows")
	if err != nil {
		return err
	}
	switch {
	case action == hookAdded:
		if dryRun {
			_, _ = fmt.Fprintf(w, "  ~ would add Stop hook: %s\n", hookCmd)
		} else {
			_, _ = fmt.Fprintf(w, "  + added Stop hook: %s\n", hookCmd)
		}
	case action == hookMigrated || contractMigrated:
		if dryRun {
			_, _ = fmt.Fprintf(w, "  ~ would migrate Stop hook to current invocation: %s\n", hookCmd)
		} else {
			_, _ = fmt.Fprintf(w, "  + migrated Stop hook to current invocation: %s\n", hookCmd)
		}
	default:
		_, _ = fmt.Fprintln(w, "  ✓ Stop hook already configured")
	}
	return nil
}

// workMomentMatcher is the PostToolUse matcher of the edit hook: a list of exact
// tool names, which the host matches exactly.
const workMomentMatcher = "Edit|Write|MultiEdit"

// ensureWorkingMomentHooks registers the two working-moment hooks: the
// message-submit one (UserPromptSubmit, no matcher support) and the edit one
// (PostToolUse, matched to Edit|Write|MultiEdit). Idempotent and
// non-destructive like the hooks above: a re-run adds nothing, an existing
// entry for the event is never duplicated or rewritten beyond a stale ghost
// binary path, and every other hook is left alone. A dry run reports what it
// would add and writes nothing.
func ensureWorkingMomentHooks(w io.Writer, sf *settingsFile, ghostBin string, dryRun bool) error {
	for _, h := range []struct {
		event, matcher, token, label string
	}{
		{"UserPromptSubmit", "", "message-submit", "message-submit"},
		{"PostToolUse", workMomentMatcher, "edit", "edit"},
	} {
		hookCmd := shellQuote(ghostBin) + " hook " + h.token + " --source claude-code"
		action, err := reconcileHookMatcher(sf, h.event, h.matcher, "hook "+h.token, hookCmd, hookCmd, runtime.GOOS == "windows")
		if err != nil {
			return err
		}
		switch action {
		case hookAdded:
			if dryRun {
				_, _ = fmt.Fprintf(w, "  ~ would add %s hook (%s): %s\n", h.event, h.label, hookCmd)
			} else {
				_, _ = fmt.Fprintf(w, "  + added %s hook (%s): %s\n", h.event, h.label, hookCmd)
			}
		case hookMigrated:
			if dryRun {
				_, _ = fmt.Fprintf(w, "  ~ would migrate %s hook to current invocation: %s\n", h.event, hookCmd)
			} else {
				_, _ = fmt.Fprintf(w, "  + migrated %s hook to current invocation: %s\n", h.event, hookCmd)
			}
		default:
			_, _ = fmt.Fprintf(w, "  ✓ %s hook already configured\n", h.event)
		}
	}
	return nil
}

// migratePreContractHook rewrites every hook whose leading token is a ghost
// binary and whose argument tail is exactly the pre-contract bare form (e.g.
// `… hook stop` with no flags) into desiredCmd, which carries `--source
// claude-code`. The contract has no legacy mode, so a stale bare invocation
// fails open on every fire until rewritten here; this migration keeps
// `ghost mcp init` (idempotent, non-destructive) the one-step fix. Every
// registered command is examined — not just the first substring match — so a
// stale duplicate sorting ahead of the real bare hook can never mask it.
// Matching is exact on the arg tail, and the leading token must be a ghost
// binary: a hand-edited wrapper that adds redirections or env vars, or another
// tool whose command happens to end in the same tail, never matches and is
// left untouched — the same executable test isStaleGhostHookCommand applies.
// Reports whether any migration happened.
func migratePreContractHook(sf *settingsFile, event, bareTail, desiredCmd string) (bool, error) {
	cmds, err := sf.hookCommands(event)
	if err != nil || len(cmds) == 0 {
		return false, err
	}
	migrated := false
	seen := make(map[string]bool, len(cmds))
	for _, cmd := range cmds {
		if seen[cmd] {
			continue // replaceHookCommand already rewrote every exact instance
		}
		seen[cmd] = true
		bin, rest, ok := splitHookCommand(cmd)
		if !ok || !isGhostBinaryName(bin) || rest != bareTail {
			continue
		}
		changed, err := sf.replaceHookCommand(event, cmd, desiredCmd)
		if err != nil {
			return migrated, fmt.Errorf("migrate %s hook to contract invocation: %w", event, err)
		}
		migrated = migrated || changed
	}
	return migrated, nil
}

// warnPercentPath flags ghost binary paths containing '%' on Windows: cmd.exe
// may substitute %NAME% sequences when the hook command runs, silently
// mangling the path. There's no escape that's safe to apply blindly — %%
// only collapses inside batch-file parsing, not a plain `cmd /c "..."`
// invocation — so this warns instead of guessing.
func warnPercentPath(w io.Writer, ghostBin string) {
	if runtime.GOOS == "windows" && strings.Contains(ghostBin, "%") {
		_, _ = fmt.Fprintf(w, "  ! warning: ghost binary path %q contains '%%', which cmd.exe may substitute as an environment variable when the hook runs — consider installing ghost to a path without '%%'\n", ghostBin)
	}
}

// ensureAutoMemoryDisabled sets "autoMemoryEnabled": false in settings.json so
// Claude Code stops writing its own flat-file memory.  Without this flag,
// Claude Code maintains ~/.claude/projects/*/memory/ markdown files that
// conflict with Ghost: they inject stale or duplicate context at session start
// and cause Claude to consult file memory before Ghost's richer store.
//
// The operation is idempotent — re-running init when the flag is already false
// is a no-op.  The flag only suppresses Claude Code's built-in file-memory
// writes; Ghost's own SessionStart hook and MCP tools are unaffected.
func ensureAutoMemoryDisabled(w io.Writer, sf *settingsFile, dryRun bool) error {
	if dryRun {
		v, present := sf.getAutoMemoryEnabled()
		if !present || v {
			_, _ = fmt.Fprintln(w, "  ~ would set autoMemoryEnabled: false")
		} else {
			_, _ = fmt.Fprintln(w, "  ✓ autoMemoryEnabled already false")
		}
		return nil
	}

	changed, err := sf.setAutoMemoryEnabled(false)
	if err != nil {
		return fmt.Errorf("set autoMemoryEnabled: %w", err)
	}
	if changed {
		_, _ = fmt.Fprintln(w, "  + set autoMemoryEnabled: false (disables competing file-memory)")
	} else {
		_, _ = fmt.Fprintln(w, "  ✓ autoMemoryEnabled already false")
	}
	return nil
}

type projectInfo struct {
	ID   string
	Path string
	Name string
}

// importMemories opens the Ghost DB and imports Claude Code memory files
// for all known projects.
//
// A dry run resolves the directory WITHOUT creating it (config.DataDirPath) and
// opens the store READ-ONLY (memory.OpenReadDB), because a preview must not be
// the write it previews (#1049): OpenDB runs migrations and writes a
// pre-migration copy, so the run that ends "No changes made (dry run)" would
// have made an irreversible one, and an older store it migrated would then
// refuse a live server's writes. The ordinary run keeps the read-write open,
// because importing memories is a real write.
func importMemories(w io.Writer, dryRun bool) ([]projectInfo, error) {
	// A GHOST_DEV_FORBID_DATA_DIR refusal arrives from DataDirPath, before
	// anything is opened or created (#721). `ghost mcp init` is a command, not
	// a hook, so the refusal is an error it reports rather than an empty session.
	var dataDir string
	var err error
	if dryRun {
		dataDir, err = config.DataDirPath()
	} else {
		dataDir, err = config.DataDir()
	}
	if err != nil {
		return nil, fmt.Errorf("data dir: %w", err)
	}
	dbPath := filepath.Join(dataDir, "ghost.db")

	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		_, _ = fmt.Fprintln(w, "  - no Ghost database found (memories import automatically on first session)")
		return nil, nil
	}

	var db *sql.DB
	if dryRun {
		db, err = memory.OpenReadDB(dbPath)
	} else {
		db, err = memory.OpenDB(dbPath)
	}
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	defer db.Close() //nolint:errcheck

	// A read-only preview cannot run the migrations a store behind this build
	// needs, and selecting from it would fail on a column it does not have, so
	// the preview is skipped rather than attempted. The message names the one
	// write that makes the preview possible.
	if dryRun {
		version, verr := memory.DBUserVersion(db)
		if verr != nil {
			return nil, fmt.Errorf("read schema version: %w", verr)
		}
		if version != memory.SchemaVersion() {
			if version > memory.SchemaVersion() {
				_, _ = fmt.Fprintf(w, "  - database is at schema v%d, newer than this Ghost (v%d) — upgrade ghost before previewing an import\n",
					version, memory.SchemaVersion())
			} else {
				_, _ = fmt.Fprintf(w, "  - database is at schema v%d (this Ghost reads v%d) — a dry run does not migrate it; start a session or run `ghost mcp init`, then preview again\n",
					version, memory.SchemaVersion())
			}
			return nil, nil
		}
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := memory.NewStore(db, logger)

	ctx := context.Background()
	projects, err := store.ListProjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}

	var infos []projectInfo
	for _, p := range projects {
		infos = append(infos, projectInfo{ID: p.ID, Path: p.Path, Name: p.Name})

		if !filepath.IsAbs(p.Path) {
			continue
		}

		if dryRun {
			_, _ = fmt.Fprintf(w, "  ~ %s — would scan for importable memories\n", p.Name)
			continue
		}

		n, refused, failed, notRegular, err := claudeimport.Import(ctx, store, p.ID, p.Path, logger)
		if err != nil {
			_, _ = fmt.Fprintf(w, "  ! %s — import error: %v\n", p.Name, err)
			continue
		}
		if n > 0 {
			_, _ = fmt.Fprintf(w, "  ✓ %s — %d memories imported\n", p.Name, n)
		} else {
			_, _ = fmt.Fprintf(w, "  - %s — no new memories\n", p.Name)
		}
		for _, f := range refused {
			_, _ = fmt.Fprintf(w, "  ! %s — refused by the token guard: %s\n", p.Name, memory.ProjectArg("file", f))
		}
		for _, f := range failed {
			_, _ = fmt.Fprintf(w, "  ! %s — could not store (a later run retries it): %s\n", p.Name, memory.ProjectArg("file", f))
		}
		for _, f := range notRegular {
			_, _ = fmt.Fprintf(w, "  ! %s — skipped, not a regular file (never read): %s\n", p.Name, memory.ProjectArg("file", f))
		}
	}

	return infos, nil
}

// staleRedirectSeedLiteral mirrors the synthetic older redirect used by the
// atomic-write test fixture, so a redirect in that exact shape is still
// recognised as Ghost's own and refreshed. It is not a template Ghost ever
// generated; it stands in for one in tests.
const staleRedirectSeedLiteral = "# Myproject Project Memory\n\nAll project knowledge is stored in Ghost.\nCall `ghost_list_projects` to see what is known.\n"

// sanitizeName allowlists safe characters for project names interpolated into
// MEMORY.md files that Claude Code auto-loads (prevents prompt injection).
func sanitizeName(name string) string {
	var sb strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' || r == ' ' || r == '.' {
			sb.WriteRune(r)
		}
	}
	s := sb.String()
	if len(s) > 64 {
		s = s[:64]
	}
	if s == "" {
		s = "unknown"
	}
	return s
}

// writeRedirects creates MEMORY.md redirect files in Claude's project memory
// directories for each known Ghost project. The write goes through
// writeFileAtomic: refreshing ghost's own redirect must not leave a
// half-written file behind for the next session start to read.
func writeRedirects(w io.Writer, projects []projectInfo, dryRun bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		_, _ = fmt.Fprintf(w, "  ! cannot determine home directory: %v\n", err)
		return
	}

	for _, p := range projects {
		if !filepath.IsAbs(p.Path) {
			continue
		}

		encoded := claudeimport.EncodeProjectPath(p.Path)
		dir := filepath.Join(home, ".claude", "projects", encoded, "memory")
		target := filepath.Join(dir, "MEMORY.md")

		// Check if redirect already exists and is current.
		if data, err := os.ReadFile(target); err == nil {
			content := string(data)
			safeName := sanitizeName(p.Name)
			currentContent := fmt.Sprintf(`# %s Project Memory

All project knowledge is stored in Ghost and injected automatically at session start via the SessionStart hook.
Project context (memories + summary) is already in your system prompt — no tool calls needed to load it.

Use `+"`ghost_memory_save`"+` to save new discoveries during work.
Use `+"`ghost_memory_search`"+` to search for specific facts.
`, safeName)
			oldContent := fmt.Sprintf(`# %s Project Memory

All project knowledge is stored in Ghost. At session start, run:
1. `+"`ghost_list_projects`"+` to discover projects
2. `+"`ghost_project_context`"+` with project_id "%s" to load accumulated knowledge
`, safeName, safeName)

			if content == currentContent {
				_, _ = fmt.Fprintf(w, "  ✓ %s — redirect exists\n", p.Name)
				continue
			}
			// Rewrite only an unedited old-format Ghost redirect. Anything else
			// (including a redirect the user has edited) is left alone, the same
			// way an unrecognised MEMORY.md is.
			if content != oldContent && content != staleRedirectSeedLiteral {
				_, _ = fmt.Fprintf(w, "  - %s — MEMORY.md exists (not overwriting)\n", p.Name)
				continue
			}
			// Old Ghost redirect with stale tool-call instructions — update it.
		}

		if dryRun {
			_, _ = fmt.Fprintf(w, "  ~ %s — would create redirect\n", p.Name)
			continue
		}

		if err := os.MkdirAll(dir, 0755); err != nil {
			_, _ = fmt.Fprintf(w, "  ! %s — mkdir error: %v\n", p.Name, err)
			continue
		}

		safeName := sanitizeName(p.Name)
		content := fmt.Sprintf(`# %s Project Memory

All project knowledge is stored in Ghost and injected automatically at session start via the SessionStart hook.
Project context (memories + summary) is already in your system prompt — no tool calls needed to load it.

Use `+"`ghost_memory_save`"+` to save new discoveries during work.
Use `+"`ghost_memory_search`"+` to search for specific facts.
`, safeName)

		if err := writeFileAtomic(target, []byte(content), 0644); err != nil {
			_, _ = fmt.Fprintf(w, "  ! %s — write error: %v\n", p.Name, err)
			continue
		}
		_, _ = fmt.Fprintf(w, "  + %s — created redirect\n", p.Name)
	}
}
