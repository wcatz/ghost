package mcpinit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/hostevent"
)

// Codex integration (spec §4 Phase 2). Ghost installs two artifacts, both
// additive merges that never rewrite content they don't own:
//
//   - `[mcp_servers.ghost]` is merged TEXTUALLY into ~/.codex/config.toml.
//     The user's config may contain comments and hand-tuned tables, so the
//     file is never parsed and re-marshaled: the ghost table block is located
//     line-wise, spliced in at the end (or replaced in place on drift), and
//     every other byte of the file is preserved exactly.
//
//   - Lifecycle hooks are merged JSON-wise into ~/.codex/hooks.json, wiring
//     SessionStart/Stop/SessionEnd onto `ghost hook <event> --source codex`.
//
// Verified against primary sources (2026-08-24, developers.openai.com/codex/hooks):
//
//   - Codex discovers hooks at ~/.codex/hooks.json (user scope) or inline
//     [hooks] tables in config.toml; mixing both forms in one layer warns at
//     startup, so a dedicated hooks.json is the clean seam. Event names match
//     the Claude dialect (SessionStart, Stop, SessionEnd, …) and stdin
//     payloads share the dialect fields verbatim (session_id,
//     transcript_path, cwd, hook_event_name, plus host extras that
//     hostevent.Parse tolerates), so no shim sits between codex and ghost.
//   - Non-managed hooks are SKIPPED until the user reviews and trusts the
//     exact definition via /hooks in the CLI (trust is keyed to the hook's
//     hash). Install must tell the user to run /hooks — until then codex
//     silently drops our events.
//   - SessionEnd defaults to a 1s timeout capped at 3s; ghost's handlers only
//     spawn detached workers, but the maximum is configured explicitly so a
//     slow spawn burst can't truncate the session-end pass.
//
// CODEX_HOME relocates the whole directory (config, credentials, hooks), so
// both paths resolve through codexHomeDir.

// codexSessionEndTimeoutSec is the documented maximum SessionEnd budget.
// Codex defaults SessionEnd to 1s; ghost's spawn-and-return handler fits
// easily but deserves the full headroom on a loaded machine.
const codexSessionEndTimeoutSec = 3

// codexMCPServerKey is the config.toml table ghost owns. Anything under it
// (including sub-tables like [mcp_servers.ghost.env]) is ghost's entry;
// everything else in the file is the user's and is never touched.
const codexMCPServerKey = "mcp_servers.ghost"

// codexHooksDescription is written into freshly-created hooks.json files.
// An existing description is preserved untouched — the field is optional
// metadata and not worth a rewrite war.
const codexHooksDescription = "Ghost persistent memory lifecycle hooks (managed by `ghost mcp init --client codex`)"

// codexHookAction is one command handler in hooks.json. Only type "command"
// handlers run today. Timeout is seconds; omitted means codex's default.
type codexHookAction struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

// codexHookRule omits matcher deliberately: lifecycle events have no useful
// matcher target (SessionStart matcher filters start source, SessionEnd
// filters end reason — omitting matches every fire, which is what the
// contract wants).
type codexHookRule struct {
	Hooks []codexHookAction `json:"hooks"`
}

// codexLifecycleEvents pairs each hooks.json event key with its contract-v1
// argv token. Codex shares the Claude event names verbatim.
var codexLifecycleEvents = []struct {
	Key        string
	EventToken string
}{
	{"SessionStart", string(hostevent.EventSessionStart)},
	{"Stop", string(hostevent.EventStop)},
	{"SessionEnd", string(hostevent.EventSessionEnd)},
}

// codexHomeDir resolves the directory codex reads configuration from:
// $CODEX_HOME when set, else ~/.codex.
func codexHomeDir() (string, error) {
	if dir := os.Getenv("CODEX_HOME"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return filepath.Join(home, ".codex"), nil
}

func codexConfigTomlPath() (string, error) {
	dir, err := codexHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.toml"), nil
}

func codexHooksPath() (string, error) {
	dir, err := codexHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "hooks.json"), nil
}

// codexTOMLString renders s as a TOML string value. Literal strings (single
// quotes) are preferred — no escape processing, so Windows backslash paths
// stay readable — falling back to an escaped basic string when the path
// itself contains a single quote or a line break.
func codexTOMLString(s string) string {
	if !strings.ContainsAny(s, "'\n\r") {
		return "'" + s + "'"
	}
	esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`)
	return `"` + esc.Replace(s) + `"`
}

// decodeCodexTOMLString reverses codexTOMLString for the simple forms init
// itself writes (and hand edits typically produce). Unquoted values are
// returned as-is; the caller treats an unresolvable value as drift.
func decodeCodexTOMLString(val string) string {
	val = strings.TrimSpace(val)
	if len(val) >= 2 && val[0] == '\'' && val[len(val)-1] == '\'' {
		return val[1 : len(val)-1]
	}
	if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
		inner := val[1 : len(val)-1]
		var sb strings.Builder
		for i := 0; i < len(inner); i++ {
			if inner[i] == '\\' && i+1 < len(inner) {
				i++
				switch inner[i] {
				case 'n':
					sb.WriteByte('\n')
				case 'r':
					sb.WriteByte('\r')
				case 't':
					sb.WriteByte('\t')
				default:
					sb.WriteByte(inner[i])
				}
				continue
			}
			sb.WriteByte(inner[i])
		}
		return sb.String()
	}
	return val
}

// parseCodexTOMLStringArray decodes a flat TOML array of strings such as
// ["mcp"]. Multi-line or bare-token arrays return nil, which callers treat
// as drift rather than trying to emulate a full TOML parser.
func parseCodexTOMLStringArray(val string) []string {
	val = strings.TrimSpace(val)
	if !strings.HasPrefix(val, "[") || !strings.HasSuffix(val, "]") {
		return nil
	}
	inner := val[1 : len(val)-1]
	var out []string
	i := 0
	for i < len(inner) {
		switch c := inner[i]; c {
		case '\'', '"':
			var sb strings.Builder
			j := i + 1
			closed := false
			for j < len(inner) {
				if c == '"' && inner[j] == '\\' && j+1 < len(inner) {
					sb.WriteString(decodeCodexTOMLString(`"` + string(inner[j+1]) + `"`))
					j += 2
					continue
				}
				if inner[j] == c {
					closed = true
					break
				}
				sb.WriteByte(inner[j])
				j++
			}
			if !closed {
				return nil
			}
			out = append(out, sb.String())
			i = j + 1
		case ',', ' ', '\t', '\r':
			i++
		default:
			return nil
		}
	}
	return out
}

// codexIsTableHeader reports whether line opens a TOML table. The '=' guard
// keeps an array element like ["a","b"] inside a multi-line value from being
// mistaken for a header; real headers never carry an assignment.
func codexIsTableHeader(line string) bool {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "[") || !strings.HasSuffix(trimmed, "]") {
		return false
	}
	return !strings.Contains(trimmed, "=")
}

// codexTableName extracts the table name from a header line, handling both
// [table] and [[array-of-tables]] forms.
func codexTableName(line string) string {
	t := strings.TrimSpace(line)
	t = strings.TrimPrefix(t, "[[")
	t = strings.TrimPrefix(t, "[")
	if end := strings.Index(t, "]"); end >= 0 {
		t = t[:end]
	}
	return strings.TrimSpace(t)
}

// findCodexTOMLTable locates the [key] table (including its sub-tables,
// [key.*]) in lines. It returns the half-open line span [start, end) covering
// the table body — end excludes trailing blank/comment lines so the spacing
// and lead-in comments of the next section survive any replacement.
func findCodexTOMLTable(lines []string, key string) (start, end int, ok bool) {
	for i, line := range lines {
		if !codexIsTableHeader(line) {
			continue
		}
		name := codexTableName(line)
		if name != key && !strings.HasPrefix(name, key+".") {
			continue
		}
		j := i + 1
		for j < len(lines) {
			if codexIsTableHeader(lines[j]) {
				next := codexTableName(lines[j])
				if next != key && !strings.HasPrefix(next, key+".") {
					break
				}
			}
			j++
		}
		for j > i+1 {
			trimmed := strings.TrimSpace(lines[j-1])
			if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
				break
			}
			j--
		}
		return i, j, true
	}
	return 0, 0, false
}

// codexMCPServerValues holds the parts of a [mcp_servers.ghost] table that
// decide whether it is current: the resolved command and its args.
type codexMCPServerValues struct {
	Command string
	Args    []string
}

// parseCodexMCPServerBlock extracts command/args from a ghost table's lines.
// Comments, blank lines, and sub-tables ([mcp_servers.ghost.env]) are
// skipped — a hand-tuned env sub-table never makes a working registration
// read as drifted.
func parseCodexMCPServerBlock(lines []string) codexMCPServerValues {
	var vals codexMCPServerValues
	for _, line := range lines[1:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "[") {
			continue
		}
		eq := strings.Index(trimmed, "=")
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(trimmed[:eq])
		value := strings.TrimSpace(trimmed[eq+1:])
		switch key {
		case "command":
			vals.Command = decodeCodexTOMLString(value)
		case "args":
			vals.Args = parseCodexTOMLStringArray(value)
		}
	}
	return vals
}

// codexMCPCurrent reports whether the parsed ghost table registers the given
// binary with the contract's stdio invocation.
func codexMCPCurrent(vals codexMCPServerValues, ghostBin string) bool {
	return vals.Command == ghostBin && len(vals.Args) == 1 && vals.Args[0] == "mcp"
}

// renderCodexMCPServerBlock returns the exact config.toml lines ghost manages.
func renderCodexMCPServerBlock(ghostBin string) string {
	return "# Ghost persistent memory (managed by `ghost mcp init --client codex`)\n" +
		"[" + codexMCPServerKey + "]\n" +
		"command = " + codexTOMLString(ghostBin) + "\n" +
		"args = [\"mcp\"]\n"
}

// installCodexMCP merges [mcp_servers.ghost] into ~/.codex/config.toml
// without parsing the file: the ghost table block is located line-wise and
// either left alone (current), spliced in at the end (absent), or replaced in
// place (stale command/args — e.g. after an upgrade moved the binary). Every
// byte outside the ghost block is preserved, comments included.
func installCodexMCP(w io.Writer, ghostBin string, dryRun bool) (bool, error) {
	path, err := codexConfigTomlPath()
	if err != nil {
		return false, err
	}

	existing, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		want := renderCodexMCPServerBlock(ghostBin)
		if dryRun {
			_, _ = fmt.Fprintf(w, "  ~ would create config.toml with the ghost MCP server (%s)\n", path)
			return true, nil
		}
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return false, fmt.Errorf("create codex dir: %w", err)
		}
		if err := os.WriteFile(path, []byte(want), 0644); err != nil {
			return false, fmt.Errorf("write config.toml: %w", err)
		}
		_, _ = fmt.Fprintf(w, "  + created config.toml with the ghost MCP server (%s)\n", path)
		return true, nil
	} else if err != nil {
		return false, fmt.Errorf("read config.toml: %w", err)
	}

	lines := strings.Split(string(existing), "\n")
	start, end, found := findCodexTOMLTable(lines, codexMCPServerKey)
	if !found {
		// Absent table: degrade to append-at-end.
		start, end = len(lines), len(lines)
	} else if codexMCPCurrent(parseCodexMCPServerBlock(lines[start:end]), ghostBin) {
		_, _ = fmt.Fprintf(w, "  ✓ ghost MCP server already registered in config.toml\n")
		return false, nil
	}

	if dryRun {
		if found {
			_, _ = fmt.Fprintf(w, "  ~ would repair the ghost MCP server entry in config.toml (%s)\n", path)
		} else {
			_, _ = fmt.Fprintf(w, "  ~ would register the ghost MCP server in config.toml (%s)\n", path)
		}
		return true, nil
	}

	// Assemble from the text around the managed span: everything before the
	// ghost table and everything after its last owned line. Absent spans
	// degrade to an append-at-end. Only ever ADDS the newline characters
	// needed to give our block one blank line of separation on each side —
	// existing newlines are content and stay put.
	prefix := strings.Join(lines[:start], "\n")
	if prefix != "" {
		if !strings.HasSuffix(prefix, "\n") {
			prefix += "\n"
		}
		if !strings.HasSuffix(prefix, "\n\n") {
			prefix += "\n"
		}
	}
	suffix := ""
	if found {
		suffix = strings.Join(lines[end:], "\n")
		if suffix != "" && !strings.HasPrefix(suffix, "\n") {
			suffix = "\n" + suffix
		}
	}

	content := prefix + renderCodexMCPServerBlock(ghostBin) + suffix
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return false, fmt.Errorf("write config.toml: %w", err)
	}
	if found {
		_, _ = fmt.Fprintf(w, "  + repaired the ghost MCP server entry in config.toml (%s)\n", path)
	} else {
		_, _ = fmt.Fprintf(w, "  + registered the ghost MCP server in config.toml (%s)\n", path)
	}
	return true, nil
}

// renderCodexHooksConfig returns a complete hooks.json document for machines
// without one, rendered with the resolved absolute ghost binary path.
func renderCodexHooksConfig(ghostBin string) string {
	events := make(map[string][]codexHookRule, len(codexLifecycleEvents))
	for _, ev := range codexLifecycleEvents {
		events[ev.Key] = []codexHookRule{{Hooks: []codexHookAction{renderCodexHookAction(ev.EventToken, ghostBin)}}}
	}
	doc := map[string]any{
		"description": codexHooksDescription,
		"hooks":       events,
	}
	return marshalCodexJSON(doc)
}

// renderCodexHookAction renders one handler for a contract event token. Only
// SessionEnd carries an explicit timeout (its codex default of 1s is tighter
// than the others' 600s).
func renderCodexHookAction(eventToken, ghostBin string) codexHookAction {
	action := codexHookAction{
		Type:    "command",
		Command: shellQuote(ghostBin) + " hook " + eventToken + " --source " + string(hostevent.SourceCodex),
	}
	if eventToken == string(hostevent.EventSessionEnd) {
		action.Timeout = codexSessionEndTimeoutSec
	}
	return action
}

// marshalCodexJSON renders a config document deterministically: two-space
// indent, sorted map keys, trailing newline. Byte-stable rendering is what
// makes install idempotency and status's byte-compare work.
func marshalCodexJSON(v any) string {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(fmt.Sprintf("render codex config: %v", err))
	}
	return string(data) + "\n"
}

// isCodexGhostRule reports whether a hooks.json rule invokes the ghost binary
// (any action whose leading command token has the ghost basename). Such rules
// are ghost-managed: drifted copies are replaced, user rules referencing
// other tools are never touched.
func isCodexGhostRule(rule json.RawMessage) bool {
	var parsed struct {
		Hooks []struct {
			Command string `json:"command"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(rule, &parsed); err != nil {
		return false
	}
	for _, action := range parsed.Hooks {
		bin, _, ok := splitHookCommand(action.Command)
		if ok && isGhostBinaryName(bin) {
			return true
		}
	}
	return false
}

// codexRuleRendered renders our canonical rule for an event as raw JSON.
func codexRuleRendered(eventToken, ghostBin string) json.RawMessage {
	data, err := json.Marshal(codexHookRule{Hooks: []codexHookAction{renderCodexHookAction(eventToken, ghostBin)}})
	if err != nil {
		panic(fmt.Sprintf("render codex hook rule: %v", err))
	}
	return data
}

// codexRulePresent reports whether rules already contain want (compared
// compacted, so whitespace differences don't force a rewrite).
func codexRulePresent(rules []json.RawMessage, want json.RawMessage) bool {
	var wb bytes.Buffer
	if err := json.Compact(&wb, want); err != nil {
		return false
	}
	for _, rule := range rules {
		var rb bytes.Buffer
		if json.Compact(&rb, rule) == nil && rb.String() == wb.String() {
			return true
		}
	}
	return false
}

// mergeCodexHooksConfig overlays ghost's lifecycle rules onto an existing
// hooks.json: unknown top-level keys, the user's description, foreign events,
// and non-ghost rules are preserved verbatim; ghost-owned rules are pruned
// and our three are (re-)appended. The result is deterministically rendered,
// so feeding the output back through this function is a byte-level no-op.
func mergeCodexHooksConfig(existing []byte, ghostBin string) (string, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(existing, &top); err != nil {
		return "", fmt.Errorf("parse hooks.json: %w", err)
	}

	events := map[string]json.RawMessage{}
	if raw, ok := top["hooks"]; ok {
		if err := json.Unmarshal(raw, &events); err != nil {
			return "", fmt.Errorf("parse hooks.json hooks: %w", err)
		}
	}

	for _, ev := range codexLifecycleEvents {
		var rules []json.RawMessage
		if raw, ok := events[ev.Key]; ok {
			if err := json.Unmarshal(raw, &rules); err != nil {
				return "", fmt.Errorf("parse hooks.json %s rules: %w", ev.Key, err)
			}
		}
		kept := make([]json.RawMessage, 0, len(rules)+1)
		for _, rule := range rules {
			if !isCodexGhostRule(rule) {
				kept = append(kept, rule)
			}
		}
		ours := codexRuleRendered(ev.EventToken, ghostBin)
		if len(kept) != len(rules) || !codexRulePresent(rules, ours) {
			kept = append(kept, ours)
			rendered, err := json.Marshal(kept)
			if err != nil {
				return "", fmt.Errorf("render %s rules: %w", ev.Key, err)
			}
			events[ev.Key] = rendered
		}
	}

	out := make(map[string]json.RawMessage, len(top)+1)
	for k, v := range top {
		out[k] = v
	}
	if _, ok := out["description"]; !ok {
		desc, err := json.Marshal(codexHooksDescription)
		if err != nil {
			return "", fmt.Errorf("render description: %w", err)
		}
		out["description"] = desc
	}
	hooks, err := json.Marshal(events)
	if err != nil {
		return "", fmt.Errorf("render hooks: %w", err)
	}
	out["hooks"] = hooks

	return marshalCodexJSON(out), nil
}

// installCodexHooks wires SessionStart/Stop/SessionEnd onto the contract
// entrypoint in ~/.codex/hooks.json. An existing file is MERGED, never
// replaced: the user's own hooks (any event ghost doesn't manage) and unknown
// fields survive byte-semantically. Idempotent via byte-compare against the
// deterministically rendered desired document.
func installCodexHooks(w io.Writer, ghostBin string, dryRun bool) (bool, error) {
	path, err := codexHooksPath()
	if err != nil {
		return false, err
	}

	existing, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if dryRun {
			_, _ = fmt.Fprintf(w, "  ~ would install hooks.json (%s)\n", path)
			return true, nil
		}
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return false, fmt.Errorf("create codex dir: %w", err)
		}
		if err := os.WriteFile(path, []byte(renderCodexHooksConfig(ghostBin)), 0644); err != nil {
			return false, fmt.Errorf("write hooks.json: %w", err)
		}
		_, _ = fmt.Fprintf(w, "  + installed hooks.json (%s)\n", path)
		return true, nil
	} else if err != nil {
		return false, fmt.Errorf("read hooks.json: %w", err)
	}

	desired, err := mergeCodexHooksConfig(existing, ghostBin)
	if err != nil {
		return false, err
	}
	if string(existing) == desired {
		_, _ = fmt.Fprintf(w, "  ✓ hooks already wired (%s)\n", path)
		return false, nil
	}
	if dryRun {
		_, _ = fmt.Fprintf(w, "  ~ would update hooks.json (%s)\n", path)
		return true, nil
	}
	if err := os.WriteFile(path, []byte(desired), 0644); err != nil {
		return false, fmt.Errorf("write hooks.json: %w", err)
	}
	_, _ = fmt.Fprintf(w, "  + updated hooks.json (%s)\n", path)
	return true, nil
}

// loadCodexHooksRules parses the installed hooks.json into per-event rule
// lists for status. Missing or malformed files surface as errors so status
// can report them as actionable failures.
func loadCodexHooksRules() (map[string][]json.RawMessage, error) {
	path, err := codexHooksPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read hooks.json: %w", err)
	}
	var top struct {
		Hooks map[string]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("parse hooks.json: %w", err)
	}
	out := make(map[string][]json.RawMessage, len(codexLifecycleEvents))
	for _, ev := range codexLifecycleEvents {
		raw, ok := top.Hooks[ev.Key]
		if !ok {
			out[ev.Key] = nil
			continue
		}
		var rules []json.RawMessage
		if err := json.Unmarshal(raw, &rules); err != nil {
			return nil, fmt.Errorf("parse hooks.json %s rules: %w", ev.Key, err)
		}
		out[ev.Key] = rules
	}
	return out, nil
}

// codexContractHookWired is the codex analog of contractHookWired: it walks
// codex's OWN hooks.json rules (not Claude's settings.json) and reports
// whether any command invokes `hook <eventToken>` with an exact --source
// codex token, accepting both "--source X" and "--source=X" flag forms.
// Token-based, not substring-based — a lookalike like "--source codex-extra"
// would fail hostevent.Parse at runtime, so it must never read as wired.
func codexContractHookWired(rules []json.RawMessage, eventToken, wantSource string) bool {
	for _, rule := range rules {
		var parsed struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		}
		if err := json.Unmarshal(rule, &parsed); err != nil {
			continue
		}
		for _, action := range parsed.Hooks {
			_, rest, ok := splitHookCommand(action.Command)
			if !ok {
				continue
			}
			fields := strings.Fields(rest)
			if len(fields) < 2 || fields[0] != "hook" || fields[1] != eventToken {
				continue
			}
			for i := 2; i < len(fields); i++ {
				if fields[i] == "--source" && i+1 < len(fields) && fields[i+1] == wantSource {
					return true
				}
				if strings.HasPrefix(fields[i], "--source=") && strings.TrimPrefix(fields[i], "--source=") == wantSource {
					return true
				}
			}
		}
	}
	return false
}

// RunCodex installs Ghost's codex integration: the [mcp_servers.ghost] stdio
// entry merged textually into ~/.codex/config.toml and SessionStart/Stop/
// SessionEnd hooks merged into ~/.codex/hooks.json, both carrying the
// resolved absolute ghost binary path (desktop launchers may run codex with a
// narrower PATH than the shell that ran init). Payloads pass through
// verbatim, so no adapter artifact beyond these two config entries exists.
func RunCodex(w io.Writer, dryRun bool) error {
	if dryRun {
		_, _ = fmt.Fprintf(w, "\nDry run — showing what would change:\n\n")
	}

	// Step 1: Prerequisites — only the ghost binary is required; its resolved
	// path is baked into both artifacts.
	_, _ = fmt.Fprintln(w, "[1/4] Checking prerequisites...")
	ghostBin, _, err := checkPrereqs(w, "codex")
	if err != nil {
		return retryHint(err)
	}

	// Step 2: Ghost's own user config (not codex's).
	_, _ = fmt.Fprintln(w, "\n[2/4] Ensuring ghost config file...")
	if err := ensureConfigBootstrap(w, dryRun); err != nil {
		return retryHint(err)
	}

	// Step 3: The MCP server entry — textual TOML merge, comments preserved.
	_, _ = fmt.Fprintln(w, "\n[3/4] Merging ghost MCP server into config.toml...")
	mcpChanged, err := installCodexMCP(w, ghostBin, dryRun)
	if err != nil {
		return retryHint(err)
	}

	// Step 4: Lifecycle hooks — JSON merge into hooks.json.
	_, _ = fmt.Fprintln(w, "\n[4/4] Installing lifecycle hooks...")
	hooksChanged, err := installCodexHooks(w, ghostBin, dryRun)
	if err != nil {
		return retryHint(err)
	}

	// Codex skips non-managed hooks until the user trusts the exact
	// definitions via /hooks — a silent no-op integration if we don't say so.
	if !dryRun {
		_, _ = fmt.Fprintln(w, "\nRequired one-time step: run /hooks inside codex and approve (trust) the ghost entries — codex silently skips them until trusted.")
		if mcpChanged || hooksChanged {
			_, _ = fmt.Fprintln(w, "Restart codex to activate.")
		}
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

// codexMCPEntryStatus validates the [mcp_servers.ghost] table against the
// resolved ghost binary. The returned message explains the failure for the
// status check line.
func codexMCPEntryStatus(ghostBin string) (bool, string) {
	path, err := codexConfigTomlPath()
	if err != nil {
		return false, err.Error()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, "config.toml not found (run ghost mcp init --client codex)"
	}
	lines := strings.Split(string(data), "\n")
	start, end, found := findCodexTOMLTable(lines, codexMCPServerKey)
	if !found {
		return false, "ghost MCP server missing from config.toml (run ghost mcp init --client codex)"
	}
	if codexMCPCurrent(parseCodexMCPServerBlock(lines[start:end]), ghostBin) {
		return true, ""
	}
	return false, "ghost MCP server missing or outdated in config.toml (run ghost mcp init --client codex)"
}

// StatusCodex checks the health of the Ghost ↔ codex integration. It reports
// only codex-relevant checks — the ghost binary, the config.toml MCP entry,
// the hooks.json lifecycle wiring (token-validated), and the client-agnostic
// embedding/link stats tail shared with Status/StatusOpencode/StatusGoose.
// Claude-only checks (permissions, autoMemory, redirects) are never reported.
func StatusCodex(w io.Writer) (bool, error) {
	_, _ = fmt.Fprintf(w, "\nGhost ↔ codex integration status:\n\n")

	healthy := true
	check := func(ok bool, pass, fail string) {
		if ok {
			_, _ = fmt.Fprintf(w, "  ✓ %s\n", pass)
		} else {
			_, _ = fmt.Fprintf(w, "  ✗ %s\n", fail)
			healthy = false
		}
	}

	// 1. Ghost binary.
	ghostBin := findBinary("ghost")
	check(ghostBin != "",
		fmt.Sprintf("ghost binary: %s", ghostBin),
		"ghost binary not found in PATH")

	reportConfigFile(w)

	// 2. MCP server entry — validated against the resolved binary path, so a
	// stale baked command (binary moved on upgrade) reports unhealthy and
	// `ghost mcp init --client codex` repairs it in place.
	mcpOK, mcpFail := codexMCPEntryStatus(ghostBin)
	check(mcpOK, "ghost MCP server registered in config.toml", mcpFail)

	// 3. Lifecycle hooks — token-validated over codex's own hooks.json.
	events, herr := loadCodexHooksRules()
	if herr != nil {
		check(false, "", fmt.Sprintf("cannot read hooks: %v", herr))
	} else {
		allWired := true
		for _, ev := range codexLifecycleEvents {
			wired := codexContractHookWired(events[ev.Key], ev.EventToken, string(hostevent.SourceCodex))
			allWired = allWired && wired
			check(wired,
				ev.Key+" hook configured",
				ev.Key+" hook missing or pre-contract (run ghost mcp init --client codex)")
		}
		if allWired {
			_, _ = fmt.Fprintln(w, "  - hooks are installed; approve them once via /hooks in codex (untrusted hooks are silently skipped)")
		}
	}

	// 4. Embedding & linking health — silent embed failures leave vector
	// search and memory linking inactive.
	store := checkStoreHealth(w, check)
	if store != nil {
		defer store.Close() //nolint:errcheck
	}

	_, _ = fmt.Fprintln(w)
	if healthy {
		_, _ = fmt.Fprintln(w, "All checks passed.")
	} else {
		_, _ = fmt.Fprintln(w, "Run `ghost mcp init --client codex` to fix issues.")
	}
	return healthy, nil
}
