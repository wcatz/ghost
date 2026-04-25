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
	"strings"

	"github.com/wcatz/ghost/internal/claudeimport"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
)

// Run executes the 7-step Claude Code integration setup.
// When dryRun is true, it reports what would change without modifying anything.
func Run(w io.Writer, dryRun bool) error {
	if dryRun {
		fmt.Fprintf(w, "\nDry run — showing what would change:\n\n")
	}

	// Step 1: Prerequisites.
	_, _ = fmt.Fprintln(w, "[1/7] Checking prerequisites...")
	ghostBin, claudeBin, err := checkPrereqs(w)
	if err != nil {
		return retryHint(err)
	}

	// Step 2: MCP server registration.
	_, _ = fmt.Fprintln(w, "\n[2/7] Registering MCP server...")
	if err := registerMCP(w, ghostBin, claudeBin, dryRun); err != nil {
		return retryHint(err)
	}

	// Step 3: Tool permissions.
	_, _ = fmt.Fprintln(w, "\n[3/7] Adding tool permissions...")
	settingsFile, err := ensurePermissions(w)
	if err != nil {
		return retryHint(err)
	}

	// Step 4: SessionStart hook.
	_, _ = fmt.Fprintln(w, "\n[4/7] Configuring SessionStart hook...")
	if err := ensureHook(w, settingsFile, ghostBin); err != nil {
		return retryHint(err)
	}

	// Step 5: Disable Claude Code's built-in file memory.
	_, _ = fmt.Fprintln(w, "\n[5/7] Disabling Claude Code built-in memory...")
	if err := ensureAutoMemoryDisabled(w, settingsFile, dryRun); err != nil {
		return retryHint(err)
	}

	// Save settings (steps 3+4+5 all modify it).
	if dryRun {
		_, _ = fmt.Fprintln(w, "\n  (skipping settings write — dry run)")
	} else {
		if err := settingsFile.save(); err != nil {
			return retryHint(fmt.Errorf("save settings: %w", err))
		}
	}

	// Step 6: Import Claude Code memories.
	_, _ = fmt.Fprintln(w, "\n[6/7] Importing Claude Code memories...")
	projects, err := importMemories(w, dryRun)
	if err != nil {
		fmt.Fprintf(w, "  ! import error: %v (continuing)\n", err)
	}

	// Step 7: Project memory redirects.
	_, _ = fmt.Fprintln(w, "\n[7/7] Writing project memory redirects...")
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

// checkPrereqs verifies that both ghost and claude binaries are on PATH.
func checkPrereqs(w io.Writer) (ghostBin, claudeBin string, err error) {
	ghostBin, err = exec.LookPath("ghost")
	if err != nil {
		return "", "", fmt.Errorf("ghost binary not found in PATH — install it first")
	}
	fmt.Fprintf(w, "  ✓ ghost binary at %s\n", ghostBin)

	claudeBin, err = exec.LookPath("claude")
	if err != nil {
		return "", "", fmt.Errorf("claude CLI not found in PATH — install Claude Code first")
	}
	fmt.Fprintf(w, "  ✓ claude CLI at %s\n", claudeBin)

	return ghostBin, claudeBin, nil
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
			fmt.Fprintf(w, "  ~ would update ghost MCP server (command: %s)\n", ghostBin)
		} else {
			fmt.Fprintf(w, "  ~ would register ghost MCP server (command: %s)\n", ghostBin)
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
		fmt.Fprintf(w, "  ✓ updated ghost MCP server (command: %s)\n", ghostBin)
	} else {
		fmt.Fprintf(w, "  + registered ghost MCP server (command: %s)\n", ghostBin)
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
		fmt.Fprintf(w, "  ✓ %d permissions already present\n", existing)
	}
	for _, p := range added {
		fmt.Fprintf(w, "  + %s\n", p)
	}
	if len(added) == 0 {
		fmt.Fprintf(w, "  ✓ all %d ghost permissions configured\n", len(ghostPermissions))
	}

	return sf, nil
}

// shellQuote wraps s in POSIX single quotes, escaping any single quotes within.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ensureHook adds a SessionStart hook if not already present.
func ensureHook(w io.Writer, sf *settingsFile, ghostBin string) error {
	hookCmd := shellQuote(ghostBin) + " hook session-start"

	if sf.hasHook("SessionStart", "hook session-start") {
		_, _ = fmt.Fprintln(w, "  ✓ SessionStart hook already configured")
		return nil
	}

	entry := hookEntry{
		Matcher: "",
		Hooks: []hookAction{
			{Type: "command", Command: hookCmd},
		},
	}
	if err := sf.addHook("SessionStart", entry); err != nil {
		return fmt.Errorf("add hook: %w", err)
	}

	fmt.Fprintf(w, "  + added SessionStart hook: %s\n", hookCmd)
	return nil
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
		_, _ = fmt.Fprintln(w, "  - no Ghost database found (run ghost serve first)")
		return nil, nil
	}

	db, err := memory.OpenDB(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	defer db.Close()

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
			fmt.Fprintf(w, "  ~ %s — would scan for importable memories\n", p.Name)
			continue
		}

		n, err := claudeimport.Import(ctx, store, p.ID, p.Path, logger)
		if err != nil {
			fmt.Fprintf(w, "  ! %s — import error: %v\n", p.Name, err)
			continue
		}
		if n > 0 {
			fmt.Fprintf(w, "  ✓ %s — %d memories imported\n", p.Name, n)
		} else {
			fmt.Fprintf(w, "  - %s — no new memories\n", p.Name)
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
		fmt.Fprintf(w, "  ! cannot determine home directory: %v\n", err)
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
				fmt.Fprintf(w, "  ✓ %s — redirect exists\n", p.Name)
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
			fmt.Fprintf(w, "  ~ %s — would create redirect\n", p.Name)
			continue
		}

		if err := os.MkdirAll(dir, 0755); err != nil {
			fmt.Fprintf(w, "  ! %s — mkdir error: %v\n", p.Name, err)
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
			fmt.Fprintf(w, "  ! %s — write error: %v\n", p.Name, err)
			continue
		}
		fmt.Fprintf(w, "  + %s — created redirect\n", p.Name)
	}
}
