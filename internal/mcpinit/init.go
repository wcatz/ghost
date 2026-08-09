package mcpinit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/wcatz/ghost/internal/claudeimport"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
)

// Run executes the 8-step Claude Code integration setup.
// When dryRun is true, it reports what would change without modifying anything.
func Run(w io.Writer, dryRun bool) error {
	if dryRun {
		_, _ = fmt.Fprintf(w, "\nDry run — showing what would change:\n\n")
	}

	// Step 1: Prerequisites.
	_, _ = fmt.Fprintln(w, "[1/8] Checking prerequisites...")
	ghostBin, claudeBin, err := checkPrereqs(w, "claude")
	if err != nil {
		return retryHint(err)
	}

	// Step 2: MCP server registration.
	_, _ = fmt.Fprintln(w, "\n[2/8] Registering MCP server...")
	if err := registerMCP(w, ghostBin, claudeBin, dryRun); err != nil {
		return retryHint(err)
	}

	// Step 3: Tool permissions.
	_, _ = fmt.Fprintln(w, "\n[3/8] Adding tool permissions...")
	settingsFile, err := ensurePermissions(w)
	if err != nil {
		return retryHint(err)
	}

	// Step 4: SessionStart hook.
	_, _ = fmt.Fprintln(w, "\n[4/8] Configuring SessionStart hook...")
	if err := ensureHook(w, settingsFile, ghostBin); err != nil {
		return retryHint(err)
	}

	// Step 5: Stop hook.
	_, _ = fmt.Fprintln(w, "\n[5/8] Configuring Stop hook...")
	if err := ensureStopHook(w, settingsFile, ghostBin); err != nil {
		return retryHint(err)
	}

	// Step 6: Disable Claude Code's built-in file memory.
	_, _ = fmt.Fprintln(w, "\n[6/8] Disabling Claude Code built-in memory...")
	if err := ensureAutoMemoryDisabled(w, settingsFile, dryRun); err != nil {
		return retryHint(err)
	}

	// Save settings (steps 3-6 all modify it).
	if dryRun {
		_, _ = fmt.Fprintln(w, "\n  (skipping settings write — dry run)")
	} else {
		if err := settingsFile.save(); err != nil {
			return retryHint(fmt.Errorf("save settings: %w", err))
		}
	}

	// Step 7: Import Claude Code memories.
	_, _ = fmt.Fprintln(w, "\n[7/8] Importing Claude Code memories...")
	projects, err := importMemories(w, dryRun)
	if err != nil {
		_, _ = fmt.Fprintf(w, "  ! import error: %v (continuing)\n", err)
	}

	// Step 8: Project memory redirects.
	_, _ = fmt.Fprintln(w, "\n[8/8] Writing project memory redirects...")
	writeRedirects(w, projects, dryRun)

	if dryRun {
		_, _ = fmt.Fprintln(w, "\nNo changes made (dry run).")
	} else {
		_, _ = fmt.Fprintln(w, "\nDone! Restart Claude Code to activate.")
	}
	return nil
}

// retryHint wraps an error with a re-run suggestion.
func retryHint(err error) error {
	return fmt.Errorf("%w\n  Re-run `ghost mcp init` to retry", err)
}

// checkPrereqs verifies the binaries required for the given client target.
// The "claude" target requires both ghost and claude; the "opencode" target
// requires only ghost.
func checkPrereqs(w io.Writer, client string) (ghostBin, claudeBin string, err error) {
	ghostBin = findBinary("ghost")
	if ghostBin == "" {
		return "", "", fmt.Errorf("ghost binary not found in PATH — install it first")
	}
	_, _ = fmt.Fprintf(w, "  ✓ ghost binary at %s\n", ghostBin)

	if client == "claude" {
		claudeBin = findBinary("claude")
		if claudeBin == "" {
			return "", "", fmt.Errorf("claude CLI not found in PATH — install Claude Code first")
		}
		_, _ = fmt.Fprintf(w, "  ✓ claude CLI at %s\n", claudeBin)
	}

	return ghostBin, claudeBin, nil
}

// systemBinDirs are absolute install dirs probed after the home-relative ones.
// Tests override this to stay isolated from host binaries.
var systemBinDirs = []string{"/opt/homebrew/bin", "/usr/local/bin"}

// findBinary locates name on PATH first, then falls back to common install
// directories that are typically not on PATH (e.g. ~/.local/bin for the claude
// native installer, ~/go/bin for go install). Home-relative dirs follow the
// effective HOME; the systemBinDirs list is absolute, so tests that must stay
// isolated from host binaries override it.
func findBinary(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	for _, dir := range append([]string{
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, "go", "bin"),
	}, systemBinDirs...) {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode().Perm()&0o111 != 0 {
			return p
		}
	}
	return ""
}

// registerMCP ensures the ghost MCP server is registered with claude.
func registerMCP(w io.Writer, ghostBin, claudeBin string, dryRun bool) error {
	// Check current registration.
	out, err := exec.Command(claudeBin, "mcp", "get", "ghost").CombinedOutput()
	currentOutput := string(out)

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

	// Remove stale registration before re-adding.
	if alreadyRegistered {
		_ = exec.Command(claudeBin, "mcp", "remove", "-s", "user", "ghost").Run()
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
		return fmt.Errorf("claude mcp add-json: %s: %w", strings.TrimSpace(string(out)), err)
	}

	if alreadyRegistered {
		_, _ = fmt.Fprintf(w, "  ✓ updated ghost MCP server (command: %s)\n", ghostBin)
	} else {
		_, _ = fmt.Fprintf(w, "  + registered ghost MCP server (command: %s)\n", ghostBin)
	}
	return nil
}

// ensurePermissions loads settings.json and adds missing ghost permissions.
func ensurePermissions(w io.Writer) (*settingsFile, error) {
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
		_, _ = fmt.Fprintf(w, "  + %s\n", p)
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
// matches legacyCmd — the pre-#251 Windows-broken single-quoted form — and
// otherwise leaves it untouched, including hand-edited commands that merely
// differ from desiredCmd, since ghost mcp init must stay non-destructive on
// re-run. The migration match is exact, not substring, so a hand-edited
// wrapper that happens to also mention cmdSubstr is never rewritten.
func reconcileHook(sf *settingsFile, event, cmdSubstr, desiredCmd, legacyCmd string, isWindows bool) (hookReconcileAction, error) {
	_, exists, err := sf.findHookCommand(event, cmdSubstr)
	if err != nil {
		return hookUnchanged, fmt.Errorf("parse existing %s hooks: %w", event, err)
	}
	if !exists {
		entry := hookEntry{
			Matcher: "",
			Hooks: []hookAction{
				{Type: "command", Command: desiredCmd},
			},
		}
		if err := sf.addHook(event, entry); err != nil {
			return hookUnchanged, fmt.Errorf("add hook: %w", err)
		}
		return hookAdded, nil
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

	return hookUnchanged, nil
}

// ensureHook adds a SessionStart hook if not already present, or migrates it
// off the pre-#251 quoting that's broken under cmd.exe.
func ensureHook(w io.Writer, sf *settingsFile, ghostBin string) error {
	hookCmd := shellQuote(ghostBin) + " hook session-start"
	legacyCmd := shellQuotePOSIX(ghostBin) + " hook session-start"
	warnPercentPath(w, ghostBin)

	action, err := reconcileHook(sf, "SessionStart", "hook session-start", hookCmd, legacyCmd, runtime.GOOS == "windows")
	if err != nil {
		return err
	}
	switch action {
	case hookAdded:
		_, _ = fmt.Fprintf(w, "  + added SessionStart hook: %s\n", hookCmd)
	case hookMigrated:
		_, _ = fmt.Fprintf(w, "  + migrated SessionStart hook to cmd.exe-safe quoting: %s\n", hookCmd)
	default:
		_, _ = fmt.Fprintln(w, "  ✓ SessionStart hook already configured")
	}
	return nil
}

// ensureStopHook adds a Stop hook if not already present, or migrates it off
// the pre-#251 quoting that's broken under cmd.exe.
func ensureStopHook(w io.Writer, sf *settingsFile, ghostBin string) error {
	hookCmd := shellQuote(ghostBin) + " hook stop"
	legacyCmd := shellQuotePOSIX(ghostBin) + " hook stop"

	action, err := reconcileHook(sf, "Stop", "hook stop", hookCmd, legacyCmd, runtime.GOOS == "windows")
	if err != nil {
		return err
	}
	switch action {
	case hookAdded:
		_, _ = fmt.Fprintf(w, "  + added Stop hook: %s\n", hookCmd)
	case hookMigrated:
		_, _ = fmt.Fprintf(w, "  + migrated Stop hook to cmd.exe-safe quoting: %s\n", hookCmd)
	default:
		_, _ = fmt.Fprintln(w, "  ✓ Stop hook already configured")
	}
	return nil
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
func importMemories(w io.Writer, dryRun bool) ([]projectInfo, error) {
	dataDir, err := config.DataDir()
	if err != nil {
		return nil, fmt.Errorf("data dir: %w", err)
	}
	dbPath := filepath.Join(dataDir, "ghost.db")

	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		_, _ = fmt.Fprintln(w, "  - no Ghost database found (memories import automatically on first session)")
		return nil, nil
	}

	db, err := memory.OpenDB(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	defer db.Close() //nolint:errcheck

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

		n, err := claudeimport.Import(ctx, store, p.ID, p.Path, logger)
		if err != nil {
			_, _ = fmt.Fprintf(w, "  ! %s — import error: %v\n", p.Name, err)
			continue
		}
		if n > 0 {
			_, _ = fmt.Fprintf(w, "  ✓ %s — %d memories imported\n", p.Name, n)
		} else {
			_, _ = fmt.Fprintf(w, "  - %s — no new memories\n", p.Name)
		}
	}

	return infos, nil
}

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
// directories for each known Ghost project.
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
			if strings.Contains(content, "stored in Ghost") && !strings.Contains(content, "ghost_list_projects") {
				_, _ = fmt.Fprintf(w, "  ✓ %s — redirect exists\n", p.Name)
				continue
			}
			if !strings.Contains(content, "stored in Ghost") {
				// File exists with other content — don't clobber.
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

		if err := os.WriteFile(target, []byte(content), 0644); err != nil {
			_, _ = fmt.Fprintf(w, "  ! %s — write error: %v\n", p.Name, err)
			continue
		}
		_, _ = fmt.Fprintf(w, "  + %s — created redirect\n", p.Name)
	}
}
