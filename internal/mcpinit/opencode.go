package mcpinit

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
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

// opencodeGhostPluginTS is the lifecycle adapter installed under
// <config-dir>/plugins/. go:embed keeps the TypeScript source verbatim
// (template literals would fight a Go string constant); the embedded copy is
// the single source of truth for the plugin.
//
//go:embed opencode_ghost.ts
var opencodeGhostPluginTS string

// ghostBinPlaceholder in the embedded plugin is replaced with the resolved
// absolute binary path at install time: desktop launchers may run opencode
// with a narrower PATH than the shell that ran init, so the default cannot
// rely on lookup. GHOST_BIN stays the runtime override.
const ghostBinPlaceholder = "__GHOST_BIN__"

// renderOpencodeGhostPlugin returns the plugin source as it must exist on
// disk for this machine.
func renderOpencodeGhostPlugin(ghostBin string) string {
	return strings.ReplaceAll(opencodeGhostPluginTS, ghostBinPlaceholder, ghostBin)
}

// RunOpencode installs Ghost's opencode integration: one lifecycle plugin
// file that both registers the ghost MCP server (via the plugin config hook)
// and bridges idle events to `ghost hook stop --source opencode`. It never
// touches Claude Code's settings and never edits opencode's own config file —
// a single artifact under <config-dir>/plugins/, so uninstalling is
// deleting one file.
func RunOpencode(w io.Writer, dryRun bool) error {
	if dryRun {
		_, _ = fmt.Fprintf(w, "\nDry run — showing what would change:\n\n")
	}

	// Step 1: Prerequisites — only the ghost binary is required; its resolved
	// path is baked into the installed plugin.
	_, _ = fmt.Fprintln(w, "[1/3] Checking prerequisites...")
	ghostBin, _, err := checkPrereqs(w, "opencode")
	if err != nil {
		return retryHint(err)
	}

	// Step 2: Ghost's own user config (not opencode's).
	_, _ = fmt.Fprintln(w, "\n[2/3] Ensuring ghost config file...")
	if err := ensureConfigBootstrap(w, dryRun); err != nil {
		return retryHint(err)
	}

	// Step 3: The lifecycle plugin — MCP registration + stop-event bridge.
	_, _ = fmt.Fprintln(w, "\n[3/3] Installing lifecycle plugin...")
	changed, err := installOpencodePlugin(w, ghostBin, dryRun)
	if err != nil {
		return retryHint(err)
	}
	if changed && !dryRun {
		_, _ = fmt.Fprintln(w, "Restart opencode to activate.")
		verifyOpencodeRegistration(w)
	}

	// Ollama embedding model.
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

// opencodePluginPath resolves the installed lifecycle plugin file:
// <config-dir>/plugins/ghost-opencode.ts (the plural "plugins" dir is what
// opencode auto-loads), where <config-dir> is the directory opencode itself
// resolves — see opencodeConfigDir.
func opencodePluginPath() (string, error) {
	dir, err := opencodeConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "plugins", "ghost-opencode.ts"), nil
}

// installOpencodePlugin writes the lifecycle adapter to opencode's plugin
// directory, rendered with the resolved ghost binary path. Idempotent: an
// identical file is left untouched; a missing, drifted, or outdated file is
// overwritten with the embedded source.
func installOpencodePlugin(w io.Writer, ghostBin string, dryRun bool) (bool, error) {
	path, err := opencodePluginPath()
	if err != nil {
		return false, err
	}
	want := renderOpencodeGhostPlugin(ghostBin)

	if existing, err := os.ReadFile(path); err == nil && string(existing) == want {
		_, _ = fmt.Fprintf(w, "  ✓ lifecycle plugin already installed (%s)\n", path)
		return false, nil
	}

	if dryRun {
		_, _ = fmt.Fprintf(w, "  ~ would install lifecycle plugin (%s)\n", path)
		return true, nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return false, fmt.Errorf("create plugin dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(want), 0644); err != nil {
		return false, fmt.Errorf("write plugin: %w", err)
	}
	_, _ = fmt.Fprintf(w, "  + installed lifecycle plugin (%s)\n", path)
	return true, nil
}

// verifyOpencodeRegistration checks via `opencode mcp ls` that the ghost
// entry took effect. When the opencode CLI is absent it stays silent — the
// plugin is installed either way and will register on next start.
func verifyOpencodeRegistration(w io.Writer) {
	ocBin, err := exec.LookPath("opencode")
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// opencode V2 has no `mcp ls`; its `mcp list` shows only config-file
	// servers (never the plugin's mcp.transform registration) and, without
	// --standalone, starts the background service as a side effect. Nothing
	// V2 offers can verify the plugin registration, so skip it there.
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

// opencodeConfigDir returns the directory opencode resolves as its config
// directory: $OPENCODE_CONFIG_DIR when set, else $XDG_CONFIG_HOME/opencode,
// else ~/.config/opencode. opencode resolves exactly one — `opencode debug
// paths` reports config = $OPENCODE_CONFIG_DIR when it is set and
// <XDG_CONFIG_HOME>/opencode otherwise — so a check that used the XDG path
// while the environment points elsewhere would judge a file opencode never
// opens, and an adapter installed there would never load.
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

// opencodeMCPConfigNames are opencode's own config file names, examined in
// this order. opencode accepts either spelling of the same schema, so status
// reports on the first file present on disk rather than assuming one
// extension: a registration the user edited into the other file must not read
// as missing.
var opencodeMCPConfigNames = []string{"opencode.jsonc", "opencode.json"}

// opencodeConfig is the slice of opencode's config schema the registration
// check reads — only the mcp map. Every other key opencode supports is
// ignored; a file carrying JSONC comments and trailing commas still parses
// (see stripJSONC).
type opencodeConfig struct {
	MCP map[string]opencodeMCPEntry `json:"mcp"`
}

// opencodeMCPEntry is one `mcp.<name>` server: the stdio command opencode
// spawns and the flag turning it on. All three are optional in opencode's
// schema — an entry may carry only `enabled`, the shorthand for disabling a
// server — so the status checks below classify a zero entry rather than
// reject it. Every field is a pointer so the layer merge can tell "this
// layer sets the key" from "this layer leaves it alone": opencode overrides
// conflicting keys, so a higher layer writing an empty `command` replaces the
// lower layer's rather than inheriting it.
type opencodeMCPEntry struct {
	Type    *string   `json:"type"`
	Command *[]string `json:"command"`
	Enabled *bool     `json:"enabled"`
}

// opencodeMCPConfigSource is one layer of opencode's configuration: a file
// opencode reads, or inline content supplied through the environment.
type opencodeMCPConfigSource struct {
	label string // how a failure names it — its path, or $OPENCODE_CONFIG_CONTENT
	path  string // file to read; empty when data already holds the content
	data  []byte // inline content from $OPENCODE_CONFIG_CONTENT; nil for files
}

// opencodeConfigSources returns the config layers the registration check
// must judge, lowest precedence first, plus the file a missing entry should
// be added to. opencode merges these layers rather than replacing them
// (config docs: "Configuration files are merged together, not replaced") in
// the order the docs give: the file in opencode's resolved config directory
// ($OPENCODE_CONFIG_DIR when set, else $XDG_CONFIG_HOME/opencode), the custom
// path in $OPENCODE_CONFIG, and finally inline $OPENCODE_CONFIG_CONTENT. A
// layer that doesn't mention mcp.ghost must not hide the one below it, so
// every existing layer is read rather than only the first. A per-checkout
// `opencode.json` and the `.opencode` directory layers are deliberately out
// of scope: including them would make one run's verdict depend on the
// directory it was typed in, and status reports the config a user carries
// between projects.
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
			break
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
		// Nothing exists on disk: name the custom path when the user
		// declared one, else the default file opencode reads.
		if custom := os.Getenv("OPENCODE_CONFIG"); custom != "" {
			primary = custom
		} else {
			primary = filepath.Join(base, opencodeMCPConfigNames[0])
		}
	}
	return sources, primary, nil
}

// opencodeMCPEntryStatus validates the effective mcp.ghost registration —
// merged across every config layer opencode reads (see
// opencodeConfigSources) — against the resolved ghost binary, mirroring
// codexMCPEntryStatus: an empty message means the entry is current, and the
// returned message explains the failure for the status check line. The
// entry must exist, be enabled, and resolve to the ghost binary this run
// found — the ways opencode ends up running without ghost's tools while the
// lifecycle plugin sits installed and green.
//
// unjudgeable is non-empty when no verdict was reached at all, and names why:
// opencodeMCPUnreadable for a layer that could not be read or parsed, or
// opencodeMCPNoConfigDir for a config directory that could not be resolved
// (no $HOME and no $XDG_CONFIG_HOME or $OPENCODE_CONFIG_DIR). That is a
// different state from a broken entry — opencode drops a layer it cannot
// parse (probed on v2.0.15: `opencode debug config` lists no document for
// it), so the entry in such a layer is inert but unknown here, and the caller
// must not report it as covered by the plugin's registration. The two
// reasons are kept apart because only one of them names a file the user can
// repair: an unresolvable config directory has no file at all.
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
			if data, readErr = os.ReadFile(src.path); readErr != nil {
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
		// Overlay only the keys this layer sets: opencode merges configs, so
		// a higher layer overriding `enabled` must not erase the command the
		// layer below registered. Presence, not value, decides — a key the
		// layer does write wins even when its value is empty, because
		// opencode replaces conflicting keys rather than treating them as
		// absent.
		if entry.Type != nil {
			merged.Type = entry.Type
			origin.Type = src.label
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

// opencodeMCPOrigin records which config layer last set each key of the
// merged mcp.ghost entry. A merged verdict has to name the file that carries
// the offending key, not merely the last layer that mentioned the entry: with
// a lower layer disabling ghost and a higher one overriding only `command`,
// the message must point at the file holding `enabled: false`, because that is
// the file the user has to edit. Entry is the fallback for a key no layer set.
type opencodeMCPOrigin struct {
	Entry   string // last layer that mentioned mcp.ghost at all
	Type    string // layer that set `type`
	Command string // layer that set `command`
	Enabled string // layer that set `enabled`
}

// forKey returns the layer carrying key, falling back to the layer that last
// mentioned the entry when no layer set that key.
func (o opencodeMCPOrigin) forKey(key string) string {
	if key != "" {
		return key
	}
	return o.Entry
}

// opencodeMCPEntryVerdict classifies the merged mcp.ghost entry. origin names
// the layer each key came from, so every message points at the file to edit:
// each failure carries the exact edit that repairs it, because
// `ghost mcp init --client opencode` deliberately never writes opencode's
// config (see RunOpencode) — the repair is a config edit, and this line is
// where the user learns what to write.
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
	if !opencodeMCPCommandIsGhost(resolved, ghostBin) {
		return false, fmt.Sprintf("ghost MCP server command %q in %s is not the ghost binary — update mcp.ghost.command to %s",
			command[0], commandFile, opencodeMCPCommandHint(ghostBin))
	}
	return true, ""
}

// opencodeMCPEntryHint renders the whole registration to paste into the
// config file when mcp.ghost is absent.
func opencodeMCPEntryHint(ghostBin string) string {
	return fmt.Sprintf(`"mcp": {"ghost": {"type": "local", "command": %s, "enabled": true}}`,
		opencodeMCPCommandHint(ghostBin))
}

// opencodeMCPCommandHint renders the command array an mcp.ghost entry must
// carry. With no resolved binary it falls back to the bare name: the run has
// already failed the ghost-binary check, and a hint must still be something
// the user can paste.
func opencodeMCPCommandHint(ghostBin string) string {
	bin := ghostBin
	if bin == "" {
		bin = "ghost"
	}
	return fmt.Sprintf("[%q, \"mcp\"]", bin)
}

// opencodeMCPCommandIsGhost reports whether the configured command resolves
// to the ghost binary this run resolved. Identity is the file itself, not
// its spelling, so a symlink onto the resolved binary is the same
// registration rather than a different one. When ghost is not on PATH at all
// there is no reference to compare against, so the command is accepted by
// name — the ghost-binary check has already failed that run, and a config
// entry opencode can still spawn is not a second thing to report against it.
func opencodeMCPCommandIsGhost(resolved, ghostBin string) bool {
	if ghostBin == "" {
		base := filepath.Base(resolved)
		return strings.EqualFold(strings.TrimSuffix(base, filepath.Ext(base)), "ghost")
	}
	want, err := os.Stat(ghostBin)
	if err != nil {
		return false
	}
	got, err := os.Stat(resolved)
	if err != nil {
		return false
	}
	return os.SameFile(got, want)
}

// parseOpencodeConfig reads one opencode config file into the slice of the
// schema the registration check needs. opencode's own schema sets
// allowComments and allowTrailingCommas, so a file valid for opencode must
// not read as unparseable here.
func parseOpencodeConfig(data []byte) (opencodeConfig, error) {
	var cfg opencodeConfig
	if err := json.Unmarshal(stripJSONC(data), &cfg); err != nil {
		return opencodeConfig{}, err
	}
	return cfg, nil
}

// stripJSONC reduces opencode's JSONC to the plain JSON encoding/json
// accepts: comments and trailing commas removed, string literals untouched —
// a "//" or a comma inside a string is content, not structure.
func stripJSONC(src []byte) []byte {
	return stripTrailingCommas(stripJSONCComments(src))
}

// jsoncBlockEnd terminates a /* */ comment; a package-level value keeps the
// scan from rebuilding it for every comment.
var jsoncBlockEnd = []byte("*/")

// appendJSONNewlines copies only the line breaks of seg. A removed block
// comment must not shift the line number a later parse error reports, so its
// newlines survive even though its text does not.
func appendJSONNewlines(out, seg []byte) []byte {
	for _, b := range seg {
		if b == '\n' {
			out = append(out, '\n')
		}
	}
	return out
}

// stripJSONCComments drops line and block comments outside string literals,
// keeping the newlines a block comment spans so a later parse error still
// points at the right line. An unterminated block comment swallows the rest
// of the file, which is what a JSON parser would report anyway.
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

// stripTrailingCommas drops a comma whose next significant byte closes the
// container — the other half of opencode's allowTrailingCommas.
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

// isJSONSpace reports whether c is whitespace JSON ignores.
func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
