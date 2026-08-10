package mcpinit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/config"
)

// RunOpencode registers Ghost as an MCP server for opencode by merging the
// mcp.ghost entry into opencode's config file, then verifies the local Ollama
// embedding model. Unlike Run, it never touches Claude Code's settings and
// only requires the ghost binary.
func RunOpencode(w io.Writer, dryRun bool) error {
	if dryRun {
		_, _ = fmt.Fprintf(w, "\nDry run — showing what would change:\n\n")
	}

	// Step 1: Prerequisites — only the ghost binary is required.
	_, _ = fmt.Fprintln(w, "[1/3] Checking prerequisites...")
	ghostBin, _, err := checkPrereqs(w, "opencode")
	if err != nil {
		return retryHint(err)
	}

	// Step 2: Config file.
	_, _ = fmt.Fprintln(w, "\n[2/3] Ensuring config file...")
	if err := ensureConfigBootstrap(w, dryRun); err != nil {
		return retryHint(err)
	}

	// Step 3: MCP server registration.
	_, _ = fmt.Fprintln(w, "\n[3/3] Registering MCP server...")
	changed, err := registerOpencodeMCP(w, ghostBin, dryRun)
	if err != nil {
		return retryHint(err)
	}

	if changed && !dryRun {
		_, _ = fmt.Fprintln(w, "Restart opencode to activate.")
		verifyOpencodeRegistration(w)
	}

	// Step 4: Ollama embedding model.
	cfg, err := config.Load()
	if err != nil {
		_, _ = fmt.Fprintf(w, "  ! load config: %v\n", err)
	} else {
		checkOllama(w, cfg, func(ok bool, pass, fail string) {
			if ok {
				_, _ = fmt.Fprintf(w, "  ✓ %s\n", pass)
			} else {
				_, _ = fmt.Fprintf(w, "  ✗ %s\n", fail)
			}
		})
	}

	if dryRun {
		_, _ = fmt.Fprintln(w, "\nNo changes made (dry run).")
	}
	return nil
}

// registerOpencodeMCP deep-merges the ghost MCP server entry into opencode's
// config, preserving all other keys. It returns true when the config was
// (or would be, for a dry run) written.
func registerOpencodeMCP(w io.Writer, ghostBin string, dryRun bool) (bool, error) {
	path, err := opencodeConfigPath()
	if err != nil {
		return false, err
	}

	cfg, err := loadOpencodeConfig(path)
	if err != nil {
		return false, err
	}

	if mcpGhostAlreadyRegistered(cfg, ghostBin) {
		_, _ = fmt.Fprintln(w, "  ✓ ghost MCP server already registered")
		return false, nil
	}

	if dryRun {
		_, _ = fmt.Fprintln(w, "  ~ would register ghost MCP server for opencode")
		return true, nil
	}

	setMCPGhost(cfg, ghostBin)
	if strings.HasSuffix(path, ".jsonc") {
		_, _ = fmt.Fprintln(w, "  ! warning: rewriting opencode.jsonc will drop its existing comments and formatting")
	}
	if err := writeOpencodeConfig(path, cfg); err != nil {
		return false, err
	}
	_, _ = fmt.Fprintln(w, "  + registered ghost MCP server for opencode")
	return true, nil
}

// verifyOpencodeRegistration checks via `opencode mcp ls` that the ghost entry
// took effect. When the opencode CLI is absent, it points at the manual config
// route; the config-file merge has already done the work either way.
func verifyOpencodeRegistration(w io.Writer) {
	ocBin, err := exec.LookPath("opencode")
	if err != nil {
		_, _ = fmt.Fprintln(w, "opencode CLI not found in PATH — manual config route: add {\"mcp\":{\"ghost\":{\"type\":\"local\",\"command\":[\"ghost\",\"mcp\"],\"enabled\":true}}} to the opencode config")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, ocBin, "mcp", "ls").CombinedOutput()
	if err != nil {
		_, _ = fmt.Fprintf(w, "  ! could not verify registration (`opencode mcp ls` failed): %s\n", strings.TrimSpace(string(out)))
		return
	}
	if strings.Contains(string(out), "ghost") {
		_, _ = fmt.Fprintln(w, "  ✓ verified: ghost listed by `opencode mcp ls`")
	} else {
		_, _ = fmt.Fprintln(w, "  ! `opencode mcp ls` succeeded but ghost is not listed — check the config manually")
	}
}

// mcpGhostCommand reports the mcp.ghost entry's command[0] if an entry with
// command base "ghost" and command[1] "mcp" exists.
func mcpGhostCommand(cfg map[string]any) (string, bool) {
	mcp, ok := cfg["mcp"].(map[string]any)
	if !ok {
		return "", false
	}
	ghost, ok := mcp["ghost"].(map[string]any)
	if !ok {
		return "", false
	}
	cmd, ok := ghost["command"].([]any)
	if !ok || len(cmd) != 2 {
		return "", false
	}
	first, ok := cmd[0].(string)
	if !ok {
		return "", false
	}
	base := strings.ToLower(filepath.Base(first))
	base = strings.TrimSuffix(base, ".exe")
	return first, base == "ghost" && cmd[1] == "mcp"
}

// mcpGhostAlreadyRegistered reports whether the config's mcp.ghost entry uses
// `ghost mcp` as its command and points at the currently resolved ghost binary.
func mcpGhostAlreadyRegistered(cfg map[string]any, ghostBin string) bool {
	first, ok := mcpGhostCommand(cfg)
	return ok && first == ghostBin
}

// setMCPGhost deep-merges the ghost MCP server entry into the config.
func setMCPGhost(cfg map[string]any, ghostBin string) {
	mcp, ok := cfg["mcp"].(map[string]any)
	if !ok {
		mcp = make(map[string]any)
		cfg["mcp"] = mcp
	}
	mcp["ghost"] = map[string]any{
		"type":    "local",
		"command": []string{ghostBin, "mcp"},
		"enabled": true,
	}
}

// opencodeConfigPath resolves the opencode config file: opencode.jsonc when it
// already exists (opencode prefers it and loading both is undefined), else
// opencode.json.
func opencodeConfigPath() (string, error) {
	dir, err := opencodeConfigDir()
	if err != nil {
		return "", err
	}
	jsonc := filepath.Join(dir, "opencode", "opencode.jsonc")
	if _, err := os.Stat(jsonc); err == nil {
		return jsonc, nil
	}
	return filepath.Join(dir, "opencode", "opencode.json"), nil
}

// opencodeConfigDir returns $XDG_CONFIG_HOME when set, else ~/.config.
func opencodeConfigDir() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return xdg, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return filepath.Join(home, ".config"), nil
}

// loadOpencodeConfig reads and parses the config file, tolerating // line
// comments, /* block comments */, and trailing commas in .jsonc files. A
// missing or empty file yields an empty config.
func loadOpencodeConfig(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return make(map[string]any), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return make(map[string]any), nil
	}
	if strings.HasSuffix(path, ".jsonc") {
		data = stripJSONCComments(data)
	}
	cfg := make(map[string]any)
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg == nil {
		cfg = make(map[string]any)
	}
	return cfg, nil
}

// stripJSONCComments removes // line comments and /* block comments */ from
// JSONC content while leaving string contents (and URLs) intact, and drops
// trailing commas before } and ].
func stripJSONCComments(data []byte) []byte {
	var out []byte
	inStr := false
	escaped := false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if inStr {
			out = append(out, c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
			out = append(out, c)
		case '/':
			if i+1 < len(data) && data[i+1] == '/' {
				for i < len(data) && data[i] != '\n' {
					i++
				}
				if i < len(data) {
					out = append(out, '\n')
				}
			} else if i+1 < len(data) && data[i+1] == '*' {
				// Skip everything until the matching */. Nested markers are
				// not valid in JSONC, so a simple scan suffices.
				for i < len(data)-1 && (data[i] != '*' || data[i+1] != '/') {
					i++
				}
				i++ // consume the final '*' (the '/' is consumed by the loop)
			} else {
				out = append(out, c)
			}
		case '}', ']':
			// Drop a trailing comma left before this closing bracket.
			n := len(out) - 1
			for n >= 0 && (out[n] == ' ' || out[n] == '\t' || out[n] == '\n') {
				n--
			}
			if n >= 0 && out[n] == ',' {
				out = append(out[:n], out[n+1:]...)
			}
			out = append(out, c)
		default:
			out = append(out, c)
		}
	}
	return out
}

// writeOpencodeConfig writes the config atomically: temp file + rename in the
// target directory, after creating it if needed. The on-disk file preserves its
// prior mode; a new file defaults to 0600 so provider API keys stay private.
// If path is a symlink (common under stow/chezmoi dotfile setups), the write
// resolves through it and renames onto the real target instead of replacing
// the symlink itself.
func writeOpencodeConfig(path string, cfg map[string]any) error {
	writePath := path
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			writePath = resolved
		}
	}

	dir := filepath.Dir(writePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create dir %s: %w", dir, err)
	}

	mode := os.FileMode(0600)
	if st, err := os.Stat(writePath); err == nil {
		mode = st.Mode().Perm()
	}

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal opencode config: %w", err)
	}
	out = append(out, '\n')

	tmp, err := os.CreateTemp(dir, ".opencode-*.json")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()

	if _, err := tmp.Write(out); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := os.Rename(tmpPath, writePath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename temp file: %w", err)
	}
	return nil
}
