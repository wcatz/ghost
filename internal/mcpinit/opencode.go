package mcpinit

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/ai"
	"github.com/wcatz/ghost/internal/config"
)

//go:embed opencode_ghost.ts
var opencodeGhostPluginTS string

const ghostBinPlaceholder = "__GHOST_BIN__"

func renderOpencodeGhostPlugin(ghostBin string) string {
	return strings.ReplaceAll(opencodeGhostPluginTS, ghostBinPlaceholder, jsStringLiteral(ghostBin))
}

func jsStringLiteral(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return `""`
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// RunOpencode installs Ghost's opencode integration: one lifecycle plugin
// file that both registers the ghost MCP server (via the plugin config hook)
func RunOpencode(w io.Writer, dryRun bool) error {
	if dryRun {
		_, _ = fmt.Fprintf(w, "\nDry run — showing what would change:\n\n")
	}

	_, _ = fmt.Fprintln(w, "[1/3] Checking prerequisites...")
	ghostBin, _, err := checkPrereqs(w, "opencode")
	if err != nil {
		return retryHint(err)
	}

	_, _ = fmt.Fprintln(w, "\n[2/3] Ensuring ghost config file...")
	if err := ensureConfigBootstrap(w, dryRun); err != nil {
		return retryHint(err)
	}

	_, _ = fmt.Fprintln(w, "\n[3/3] Installing lifecycle plugin...")
	changed, err := installOpencodePlugin(w, ghostBin, dryRun)
	if err != nil {
		return stepError(err)
	}
	if changed && !dryRun {
		_, _ = fmt.Fprintln(w, "Restart opencode to activate.")
		verifyOpencodeRegistration(w)
	}

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

func opencodePluginPath() (string, error) {
	dir, err := opencodeConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "plugins", "ghost-opencode.ts"), nil
}

func installOpencodePlugin(w io.Writer, ghostBin string, dryRun bool) (bool, error) {
	path, err := opencodePluginPath()
	if err != nil {
		return false, err
	}
	want := renderOpencodeGhostPlugin(ghostBin)

	existing, err := readHostConfigFile(path)
	if err == nil && string(existing) == want {
		_, _ = fmt.Fprintf(w, "  ✓ lifecycle plugin already installed (%s)\n", path)
		return false, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("read %s: %w", path, err)
	}

	if dryRun {
		_, _ = fmt.Fprintf(w, "  ~ would install lifecycle plugin (%s)\n", path)
		return true, nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return false, fmt.Errorf("create plugin dir: %w", err)
	}
	if err := writeFileAtomic(path, []byte(want), 0644); err != nil {
		return false, fmt.Errorf("write plugin: %w", err)
	}
	_, _ = fmt.Fprintf(w, "  + installed lifecycle plugin (%s)\n", path)
	return true, nil
}

func verifyOpencodeRegistration(w io.Writer) {
	ocBin, err := exec.LookPath("opencode")
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if ver, err := exec.CommandContext(ctx, ocBin, "--version").Output(); err == nil && ai.OpencodeMajorVersion(string(ver)) >= 2 {
		_, _ = fmt.Fprintln(w, "  ✓ opencode V2 detected — the plugin registers ghost at startup (V2's `opencode mcp list` shows only config-file servers, so registration isn't checked here)")
		return
	}
	out, err := exec.CommandContext(ctx, ocBin, "mcp", "ls").CombinedOutput()
	if err != nil {
		_, _ = fmt.Fprintf(w, "  ! could not verify registration (`opencode mcp ls` failed): %s\n", strings.TrimSpace(string(out)))
		return
	}
	if strings.Contains(string(out), "ghost") {
		_, _ = fmt.Fprintln(w, "  ✓ verified: ghost listed by `opencode mcp ls`")
	} else {
		_, _ = fmt.Fprintln(w, "  ! `opencode mcp ls` succeeded but ghost is not listed — restart opencode, or re-run `ghost mcp init --client opencode`")
	}
}

func opencodeConfigDir() (string, error) {
	if dir := os.Getenv("OPENCODE_CONFIG_DIR"); dir != "" {
		return dir, nil
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "opencode"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return filepath.Join(home, ".config", "opencode"), nil
}

var opencodeMCPConfigNames = []string{"opencode.json", "opencode.jsonc"}

type opencodeConfig struct {
	MCP map[string]opencodeMCPEntry `json:"mcp"`
}

type opencodeMCPEntry struct {
	Type    *string   `json:"type"`
	Command *[]string `json:"command"`
	Enabled *bool     `json:"enabled"`
}

type opencodeMCPConfigSource struct {
	label string // how a failure names it — its path, or $OPENCODE_CONFIG_CONTENT
	path  string // file to read; empty when data already holds the content
	data  []byte // inline content from $OPENCODE_CONFIG_CONTENT; nil for files
}

func opencodeConfigSources() ([]opencodeMCPConfigSource, string, error) {
	base, err := opencodeConfigDir()
	if err != nil {
		return nil, "", err
	}
	primary := ""
	var sources []opencodeMCPConfigSource
	for _, name := range opencodeMCPConfigNames {
		candidate := filepath.Join(base, name)
		if _, err := os.Stat(candidate); err == nil {
			sources = append(sources, opencodeMCPConfigSource{label: candidate, path: candidate})
			primary = candidate
		}
	}
	if custom := os.Getenv("OPENCODE_CONFIG"); custom != "" {
		if _, err := os.Stat(custom); err == nil || !os.IsNotExist(err) {
			sources = append(sources, opencodeMCPConfigSource{label: custom, path: custom})
			if primary == "" {
				primary = custom
			}
		}
	}
	if content := os.Getenv("OPENCODE_CONFIG_CONTENT"); content != "" {
		sources = append(sources, opencodeMCPConfigSource{label: "$OPENCODE_CONFIG_CONTENT", data: []byte(content)})
	}
	if primary == "" {
		if custom := os.Getenv("OPENCODE_CONFIG"); custom != "" {
			primary = custom
		} else {
			primary = filepath.Join(base, opencodeMCPConfigNames[len(opencodeMCPConfigNames)-1])
		}
	}
	return sources, primary, nil
}

const (
	opencodeMCPUnreadable  = "config file"
	opencodeMCPNoConfigDir = "config directory"
)

func opencodeMCPEntryStatus(ghostBin string) (bool, string, string) {
	sources, primary, err := opencodeConfigSources()
	if err != nil {
		return false, err.Error(), opencodeMCPNoConfigDir
	}
	if len(sources) == 0 {
		return false, fmt.Sprintf("no opencode config file — add %s to %s",
			opencodeMCPEntryHint(ghostBin), primary), ""
	}
	var merged opencodeMCPEntry
	var origin opencodeMCPOrigin
	found := false
	for _, src := range sources {
		data := src.data
		if data == nil {
			var readErr error
			if data, readErr = readHostConfigFile(src.path); readErr != nil {
				return false, fmt.Sprintf("cannot read %s: %v", src.label, readErr), opencodeMCPUnreadable
			}
		}
		cfg, err := parseOpencodeConfig(data)
		if err != nil {
			return false, fmt.Sprintf("cannot parse %s as opencode config: %v", src.label, err), opencodeMCPUnreadable
		}
		entry, ok := cfg.MCP["ghost"]
		if !ok {
			continue // this layer doesn't mention ghost — lower layers still apply
		}
		if entry.Type != nil {
			merged.Type = entry.Type
		}
		if entry.Command != nil {
			merged.Command = entry.Command
			origin.Command = src.label
		}
		if entry.Enabled != nil {
			merged.Enabled = entry.Enabled
			origin.Enabled = src.label
		}
		found = true
		origin.Entry = src.label
	}
	if !found {
		return false, fmt.Sprintf("ghost MCP server missing from %s — add %s", primary, opencodeMCPEntryHint(ghostBin)), ""
	}
	ok, msg := opencodeMCPEntryVerdict(merged, origin, ghostBin)
	return ok, msg, ""
}

type opencodeMCPOrigin struct {
	Entry   string // last layer that mentioned mcp.ghost at all
	Command string // layer that set `command`
	Enabled string // layer that set `enabled`
}

func (o opencodeMCPOrigin) forKey(key string) string {
	if key != "" {
		return key
	}
	return o.Entry
}

func opencodeMCPEntryVerdict(entry opencodeMCPEntry, origin opencodeMCPOrigin, ghostBin string) (bool, string) {
	if entry.Enabled != nil && !*entry.Enabled {
		return false, fmt.Sprintf("ghost MCP server disabled in %s — set mcp.ghost.enabled to true",
			origin.forKey(origin.Enabled))
	}
	var command []string
	if entry.Command != nil {
		command = *entry.Command
	}
	commandFile := origin.forKey(origin.Command)
	if len(command) < 2 || command[1] != "mcp" {
		return false, fmt.Sprintf("ghost MCP server command in %s must be %s", commandFile, opencodeMCPCommandHint(ghostBin))
	}
	resolved, err := exec.LookPath(command[0])
	if err != nil {
		return false, fmt.Sprintf("ghost MCP server command %q in %s is not an executable file (%v)",
			command[0], commandFile, err)
	}
	if !mcpCommandIsGhost(resolved, ghostBin) {
		return false, fmt.Sprintf("ghost MCP server command %q in %s is not the ghost binary — update mcp.ghost.command to %s",
			command[0], commandFile, opencodeMCPCommandHint(ghostBin))
	}
	return true, ""
}

func opencodeMCPEntryHint(ghostBin string) string {
	return fmt.Sprintf(`"mcp": {"ghost": {"type": "local", "command": %s, "enabled": true}}`,
		opencodeMCPCommandHint(ghostBin))
}

func opencodeMCPCommandHint(ghostBin string) string {
	bin := ghostBin
	if bin == "" {
		bin = "ghost"
	}
	return fmt.Sprintf("[%q, \"mcp\"]", bin)
}

func parseOpencodeConfig(data []byte) (opencodeConfig, error) {
	var cfg opencodeConfig
	if err := leadingBOMError(data, "the file"); err != nil {
		return opencodeConfig{}, err
	}
	if err := json.Unmarshal(stripJSONC(data), &cfg); err != nil {
		return opencodeConfig{}, err
	}
	return cfg, nil
}

func stripJSONC(src []byte) []byte {
	return stripTrailingCommas(stripJSONCComments(src))
}

var jsoncBlockEnd = []byte("*/")

func appendJSONNewlines(out, seg []byte) []byte {
	for _, b := range seg {
		if b == '\n' {
			out = append(out, '\n')
		}
	}
	return out
}

func stripJSONCComments(src []byte) []byte {
	out := make([]byte, 0, len(src))
	inString, escaped := false, false
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case inString:
			out = append(out, c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
		case c == '"':
			inString = true
			out = append(out, c)
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
			if i < len(src) {
				out = append(out, '\n')
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			rest := src[i+2:]
			end := bytes.Index(rest, jsoncBlockEnd)
			if end < 0 { // unterminated: the rest of the file is comment
				out = appendJSONNewlines(out, rest)
				i = len(src)
				continue
			}
			out = appendJSONNewlines(out, rest[:end])
			i = i + 2 + end + 1 // sit on the closing "/", so the loop's i++ steps past "*/"
		default:
			out = append(out, c)
		}
	}
	return out
}

func stripTrailingCommas(src []byte) []byte {
	out := make([]byte, 0, len(src))
	inString, escaped := false, false
	for i := 0; i < len(src); i++ {
		c := src[i]
		if inString {
			out = append(out, c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			out = append(out, c)
			continue
		}
		if c == ',' {
			j := i + 1
			for j < len(src) && isJSONSpace(src[j]) {
				j++
			}
			if j < len(src) && (src[j] == '}' || src[j] == ']') {
				continue // trailing comma: dropped, not copied
			}
		}
		out = append(out, c)
	}
	return out
}

func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
