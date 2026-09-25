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
//     line-wise, spliced in at the end (or repaired in place on drift), and
//     every other byte of the file is preserved exactly. A repair rewrites
//     only the table's own `command` and `args` keys, so sub-tables such as
//     [mcp_servers.ghost.env] and any key ghost does not own survive. An entry
//     written as a dotted or inline key (`mcp_servers.ghost = {…}`) is left
//     untouched with a warning: it cannot be merged textually, and appending a
//     table beside it would be a duplicate definition.
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

// codexMCPServerKey is the config.toml table ghost owns. Inside it only the
// `command` and `args` keys are ghost's to rewrite; sub-tables
// ([mcp_servers.ghost.env]) and every other key belong to the user, and so does
// everything outside the table. None of it is ever touched.
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

// codexValueComplete reports whether a value's brackets and braces balance.
// Three things that look like structure are not: a "#" opens a comment that runs
// to the end of the line, a backslash escapes the next byte inside a basic
// "..." string, and a literal '...' string has no escapes at all. Misreading any
// of them makes a finished value look unfinished, and a repair that believes it
// is still running swallows every following line as part of it.
func codexValueComplete(value string) bool {
	depth := 0
	for i := 0; i < len(value); i++ {
		switch c := value[i]; c {
		case '#':
			return depth == 0 // the rest of the line is a comment, not a value
		case '\'', '"':
			i = codexStringEnd(value, i)
		case '[', '{':
			depth++
		case ']', '}':
			depth--
			if depth < 0 {
				return true // malformed; treat as self-contained
			}
		}
	}
	return depth == 0
}

// codexStringEnd returns the index of the quote closing the string that opens at
// i, or the last index of value when the string is unterminated. Inside a basic
// "..." string a backslash escapes the next byte, so an escaped quote does not
// close it; inside a literal '...' string nothing is escaped.
func codexStringEnd(value string, i int) int {
	quote := value[i]
	for i++; i < len(value); i++ {
		switch value[i] {
		case quote:
			return i
		case '\\':
			if quote == '"' {
				i++ // the escaped byte cannot close the string
			}
		}
	}
	return len(value) - 1
}

// codexStripComment returns line with any "#" comment removed. A "#" inside a
// quoted run belongs to the value, not to a comment.
func codexStripComment(line string) string {
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '\'', '"':
			i = codexStringEnd(line, i)
		case '#':
			return strings.TrimRight(line[:i], " \t")
		}
	}
	return line
}

// normaliseCodexTableName returns the dotted key path a table header declares,
// with each part trimmed of surrounding space and of one layer of quotes, and
// ok=false when the header cannot be read with confidence. A trailing comment
// is stripped first, so "[mcp_servers.ghost] # mine" names the same table as the
// bare spelling, and ["mcp_servers"."ghost"] and [ mcp_servers . ghost ]
// normalise to it as well. An array-of-tables header ([[name]]) is not a table
// ghost manages, so it reports false.
func normaliseCodexTableName(line string) (name string, ok bool) {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "[") || strings.HasPrefix(t, "[[") {
		return "", false
	}
	t = codexStripComment(t)
	if !strings.HasSuffix(t, "]") {
		return "", false
	}
	inner := strings.TrimSpace(t[1 : len(t)-1])
	if inner == "" {
		return "", false
	}
	parts := strings.Split(inner, ".")
	for i, part := range parts {
		part = strings.TrimSpace(part)
		if len(part) >= 2 && (part[0] == '\'' || part[0] == '"') && part[len(part)-1] == part[0] {
			part = part[1 : len(part)-1]
		}
		if part == "" || strings.ContainsAny(part, "[]") {
			return "", false
		}
		parts[i] = part
	}
	return strings.Join(parts, "."), true
}

// codexIsTableHeader reports whether line opens a TOML table. The '=' guard
// keeps an array element like ["a","b"] inside a multi-line value from being
// mistaken for a header; real headers never carry an assignment. A trailing
// comment does not disqualify a header.
func codexIsTableHeader(line string) bool {
	trimmed := codexStripComment(strings.TrimSpace(line))
	if !strings.HasPrefix(trimmed, "[") || !strings.HasSuffix(trimmed, "]") {
		return false
	}
	return !strings.Contains(trimmed, "=")
}

// codexTableName extracts the raw table name from a header line, handling both
// [table] and [[array-of-tables]] forms. Prefer normaliseCodexTableName when
// the name is compared against a key path.
func codexTableName(line string) string {
	t := codexStripComment(strings.TrimSpace(line))
	t = strings.TrimPrefix(t, "[[")
	t = strings.TrimPrefix(t, "[")
	if end := strings.Index(t, "]"); end >= 0 {
		t = t[:end]
	}
	return strings.TrimSpace(t)
}

// codexValueContinuationLines returns the indexes of lines that continue an
// unterminated value begun on an earlier line. A nested array element such as
// ["a","b"] inside a multi-line array is bracketed exactly like a header, so a
// scanner that ignores continuations can end the ghost table's span early and
// then insert owned keys that already exist further down the table.
func codexValueContinuationLines(lines []string) map[int]bool {
	continuation := make(map[int]bool)
	value, open := "", false
	for i, line := range lines {
		if !open && codexIsTableHeader(line) {
			continue // a header always closes an open value
		}
		if open {
			continuation[i] = true
			value += "\n" + line
			if codexValueComplete(value) {
				open, value = false, ""
			}
			continue
		}
		if _, v, ok := splitCodexAssignment(line); ok && !codexValueComplete(v) {
			open, value = true, v
		}
	}
	return continuation
}

// findCodexTOMLTable locates the [key] table header in lines and returns the
// half-open span [start, end) of the table's OWN key lines: everything up to
// the next table header of any name, minus trailing blank/comment lines so the
// spacing and lead-in comments of the next section survive a replacement.
// Sub-tables ([key.env]) start at their own header, so they fall outside the
// span and can never be rewritten by a caller that replaces it. The name is
// compared in normalised form, so any spelling of the key path matches, and
// lines continuing a multi-line value are skipped so a nested array element
// cannot be mistaken for the header that ends the span.
func findCodexTOMLTable(lines []string, key string) (start, end int, ok bool) {
	continuation := codexValueContinuationLines(lines)
	for i, line := range lines {
		if continuation[i] || !codexIsTableHeader(line) {
			continue
		}
		if name, named := normaliseCodexTableName(line); !named || name != key {
			continue
		}
		j := i + 1
		for j < len(lines) && (continuation[j] || !codexIsTableHeader(lines[j])) {
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

// findCodexAmbiguousGhostHeader reports a bracketed line that names the ghost
// server but cannot be matched with confidence: a key path that does not parse (a
// stray or empty key part, an unterminated header) yet still carries a "ghost"
// part, or the array-of-tables form [[mcp_servers.ghost]], which is a different
// structure from the table ghost manages. init must refuse such a file rather
// than append, because reading it as an absent table emits a second definition of
// the server, and repairing inside a table whose shape we do not understand risks
// eating the user's keys. A line that parses to a different key path is left
// alone, and a line continuing a multi-line value is not a header at all.
func findCodexAmbiguousGhostHeader(lines []string, key string) (at int, text string, ok bool) {
	leaf := key
	if i := strings.LastIndex(key, "."); i >= 0 {
		leaf = key[i+1:]
	}
	continuation := codexValueContinuationLines(lines)
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if continuation[i] || !strings.HasPrefix(trimmed, "[") {
			continue
		}
		if _, named := normaliseCodexTableName(line); named {
			continue // a parseable header: the normal path matches or ignores it
		}
		if codexHeaderNamesPart(line, leaf) {
			return i + 1, trimmed, true
		}
	}
	return 0, "", false
}

// codexHeaderNamesPart reports whether an unparseable table header still names
// the given key part, comparing with quotes and spaces removed so [a . "ghost"]
// reads the same as [a.ghost]. The check only runs on headers that failed to
// parse, and a false positive merely makes init warn instead of writing.
func codexHeaderNamesPart(line, part string) bool {
	t := strings.Map(func(r rune) rune {
		if r == '\'' || r == '"' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, codexStripComment(strings.TrimSpace(line)))
	for i := 0; i+len(part) <= len(t); i++ {
		if t[i:i+len(part)] != part {
			continue
		}
		if i > 0 && isCodexIdentByte(t[i-1]) {
			continue // part of a longer key, e.g. ghost_profile
		}
		if i+len(part) < len(t) && isCodexIdentByte(t[i+len(part)]) {
			continue
		}
		return true
	}
	return false
}

// splitCodexAssignment splits a `key = value` line into its two halves, with
// the key normalized to its bare spelling: TOML treats "command" and command as
// the same key, so a quoted owned key has to be recognized as ours rather than
// left in the file to collide with the bare one we write. ok is false for a
// line that carries no assignment (a blank, a comment, an orphaned fragment of
// a multi-line value). The first `=` separates them, which is safe because a
// TOML key can never contain one.
func splitCodexAssignment(line string) (key, value string, ok bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", "", false
	}
	eq := strings.Index(trimmed, "=")
	if eq < 0 {
		return "", "", false
	}
	key = strings.TrimSpace(trimmed[:eq])
	if len(key) >= 2 && (key[0] == '\'' || key[0] == '"') && key[len(key)-1] == key[0] {
		key = key[1 : len(key)-1]
	}
	return key, strings.TrimSpace(trimmed[eq+1:]), true
}

// codexValueLastLine returns the index of the line completing an unterminated
// value that starts on line i, or end-1 when the span runs out. Rewriting a
// multi-line owned key in place has to drop those continuation lines, or the
// orphaned fragments would corrupt the table.
func codexValueLastLine(value string, lines []string, i, end int) int {
	acc := value
	for i+1 < end {
		if codexValueComplete(acc) {
			return i
		}
		i++
		acc += "\n" + lines[i]
	}
	return i
}

// findCodexDottedGhost reports a non-table definition of the ghost MCP server:
// `mcp_servers.ghost = {…}` (or `mcp_servers.ghost.command = …`) at top level,
// `ghost = {…}` / `ghost.command = …` inside [mcp_servers], or an inline
// `mcp_servers = { ghost = {…} }`. The line-wise merge cannot read or rewrite
// any of them, so init must refuse rather than append a second definition
// beside it. It returns the 1-based line number and the offending text.
// Definitions made inside the ghost table itself are the long form this
// installer writes and are not reported.
func findCodexDottedGhost(lines []string, key string) (at int, text string, ok bool) {
	continuation := codexValueContinuationLines(lines)
	table := ""
	for i, line := range lines {
		if continuation[i] {
			continue
		}
		if codexIsTableHeader(line) {
			if name, named := normaliseCodexTableName(line); named {
				table = name
			} else {
				table = codexTableName(line) // best effort for an odd header
			}
			continue
		}
		assigned, value, isAssign := splitCodexAssignment(line)
		if !isAssign {
			continue
		}
		full := assigned
		if table != "" {
			full = table + "." + assigned
		}
		if (full == key || strings.HasPrefix(full, key+".")) && !codexTableWithin(table, key) {
			return i + 1, strings.TrimSpace(line), true
		}
		// The entry can also hide as a key of an inline mcp_servers table,
		// written either on one line or spread over several.
		if full == "mcp_servers" && codexInlineNamesKey(codexValueText(value, lines, i), "ghost") {
			return i + 1, strings.TrimSpace(line), true
		}
	}
	return 0, "", false
}

// codexValueText returns a key's value with its continuation lines folded in, so
// a multi-line inline table (mcp_servers = { … }) is scanned whole: a `ghost`
// key on a later line would otherwise read as a top-level key of its own and
// slip past the guard. An unterminated value runs to the end of the file.
func codexValueText(value string, lines []string, i int) string {
	if codexValueComplete(value) {
		return value
	}
	return strings.Join(lines[i:codexValueLastLine(value, lines, i, len(lines))+1], " ")
}

// codexTableWithin reports whether table is key or a sub-table of it.
func codexTableWithin(table, key string) bool {
	return table == key || strings.HasPrefix(table, key+".")
}

// codexInlineNamesKey reports whether an inline-table value mentions name as a
// key (name = or name. inside the braces). The scan deliberately over-matches:
// a nested mention only causes init to warn and leave the file alone, which is
// the safe direction, while a missed one would append a duplicate table.
func codexInlineNamesKey(value, name string) bool {
	for i := 0; i < len(value); i++ {
		if q := value[i]; q == '\'' || q == '"' {
			for i++; i < len(value) && value[i] != q; i++ {
			}
			continue
		}
		if !strings.HasPrefix(value[i:], name) || (i > 0 && isCodexIdentByte(value[i-1])) {
			continue
		}
		rest := strings.TrimLeft(value[i+len(name):], " \t")
		if strings.HasPrefix(rest, "=") || strings.HasPrefix(rest, ".") {
			return true
		}
	}
	return false
}

// isCodexIdentByte reports whether c can appear in a TOML bare key, so
// `myghost` never matches a search for `ghost`.
func isCodexIdentByte(c byte) bool {
	return c == '_' || c == '-' ||
		(c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// codexMCPServerValues holds the parts of a [mcp_servers.ghost] table that
// decide whether it is current: the resolved command and its args.
type codexMCPServerValues struct {
	Command string
	Args    []string
}

// parseCodexMCPServerBlock extracts command/args from a ghost table's lines.
// Comments, blank lines, and anything that is not a `key = value` line are
// skipped, and a quoted key spelling counts as its bare form, so a hand-tuned
// table (including one carrying an env sub-table outside this span) never
// makes a working registration read as drifted.
func parseCodexMCPServerBlock(lines []string) codexMCPServerValues {
	var vals codexMCPServerValues
	for _, line := range lines[1:] {
		key, value, ok := splitCodexAssignment(line)
		if !ok {
			continue
		}
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

// codexMCPServerComment marks the block ghost owns inside a config.toml the
// user also edits by hand. It is written once, above the table.
const codexMCPServerComment = "# Ghost persistent memory (managed by `ghost mcp init --client codex`)"

// codexOwnedKey is one config.toml key inside the ghost table that ghost owns
// and is therefore allowed to rewrite. Anything else in the table (comments,
// blank lines, and keys codex supports but ghost does not manage, such as
// startup_timeout_sec or cwd) belongs to the user.
type codexOwnedKey struct {
	key  string
	line string
}

// codexMCPServerKeys returns the owned keys in the order ghost writes them, so
// a fresh install and a repair cannot drift apart.
func codexMCPServerKeys(ghostBin string) []codexOwnedKey {
	return []codexOwnedKey{
		{"command", "command = " + codexTOMLString(ghostBin) + "\n"},
		{"args", "args = [\"mcp\"]\n"},
	}
}

// codexMCPServerBlockLines returns the complete managed block: the comment, the
// table header, and the owned keys.
func codexMCPServerBlockLines(ghostBin string) []string {
	lines := []string{codexMCPServerComment, "[" + codexMCPServerKey + "]"}
	for _, k := range codexMCPServerKeys(ghostBin) {
		lines = append(lines, strings.TrimSuffix(k.line, "\n"))
	}
	return lines
}

// renderCodexMCPServerBlock returns the exact config.toml lines ghost manages.
func renderCodexMCPServerBlock(ghostBin string) string {
	return strings.Join(codexMCPServerBlockLines(ghostBin), "\n") + "\n"
}

// repairCodexMCPServerTable returns the replacement lines for a drifted ghost
// table: the canonical `command` and `args` key lines, each rewritten in place,
// with every other line of the span (comments, blank lines, the user's own
// keys) preserved verbatim and in order. A missing owned key is inserted ahead
// of the span's first real line, so a table that defines only an env sub-table
// still gets a working registration. Nothing outside the span is consulted, so
// sub-tables and the rest of the file cannot be lost.
func repairCodexMCPServerTable(lines []string, start, end int, ghostBin string) []string {
	owned := codexMCPServerKeys(ghostBin)
	ownedLine := func(key string) string {
		for _, k := range owned {
			if k.key == key {
				return strings.TrimSuffix(k.line, "\n")
			}
		}
		return ""
	}

	// What the table already defines, and where its first non-preamble line
	// is — the spot any owned key the table is missing gets inserted at.
	defined := make(map[string]bool, len(owned))
	insertAt := -1
	for i := start + 1; i < end; i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if insertAt < 0 {
			insertAt = i
		}
		if key, _, ok := splitCodexAssignment(lines[i]); ok {
			defined[key] = true
		}
	}
	if insertAt < 0 {
		insertAt = end // a table of nothing but comments: append after them
	}
	var missing []string
	for _, k := range owned {
		if !defined[k.key] {
			missing = append(missing, strings.TrimSuffix(k.line, "\n"))
		}
	}

	out := make([]string, 0, end-start+len(missing)+1)
	if start == 0 || strings.TrimSpace(lines[start-1]) != codexMCPServerComment {
		out = append(out, codexMCPServerComment) // mark the block we own
	}
	out = append(out, lines[start]) // the [mcp_servers.ghost] header, verbatim
	written := make(map[string]bool, len(owned))
	inserted := false
	for i := start + 1; i < end; i++ {
		if !inserted && i == insertAt {
			out = append(out, missing...)
			inserted = true
		}
		key, value, ok := splitCodexAssignment(lines[i])
		line := ownedLine(key)
		if !ok || line == "" {
			out = append(out, lines[i])
			continue
		}
		if written[key] {
			// A repeated key is invalid TOML; keep the first, our canonical one.
			if !codexValueComplete(value) {
				i = codexValueLastLine(value, lines, i, end)
			}
			continue
		}
		written[key] = true
		out = append(out, line)
		if !codexValueComplete(value) {
			i = codexValueLastLine(value, lines, i, end)
		}
	}
	if !inserted {
		out = append(out, missing...)
	}
	return out
}

// installCodexMCP merges [mcp_servers.ghost] into ~/.codex/config.toml
// without parsing the file: the ghost table block is located line-wise and
// either left alone (current), spliced in at the end (absent), or repaired in
// place (stale command/args — e.g. after an upgrade moved the binary). A repair
// rewrites only the ghost table's own command/args key lines; sub-tables such
// as [mcp_servers.ghost.env], the user's other keys, and every byte outside the
// ghost table are preserved exactly, comments included.
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
		if err := writeFileAtomic(path, []byte(want), 0644); err != nil {
			return false, fmt.Errorf("write config.toml: %w", err)
		}
		_, _ = fmt.Fprintf(w, "  + created config.toml with the ghost MCP server (%s)\n", path)
		return true, nil
	} else if err != nil {
		return false, fmt.Errorf("read config.toml: %w", err)
	}

	lines := strings.Split(string(existing), "\n")

	// A ghost entry written as a dotted or inline key is invisible to the
	// line-wise merge: appending a table next to it would give codex a
	// duplicate definition. Leave the file alone and say why.
	if at, text, dotted := findCodexDottedGhost(lines, codexMCPServerKey); dotted {
		_, _ = fmt.Fprintf(w, "  ! %s defines the ghost MCP server with a dotted or inline key (line %d: %s)\n", path, at, text)
		_, _ = fmt.Fprintln(w, "    ghost manages the [mcp_servers.ghost] table only, so the file was left unchanged.")
		_, _ = fmt.Fprintln(w, "    Rewrite the entry as a [mcp_servers.ghost] table and re-run, or register the server yourself.")
		return false, nil
	}

	// A header that names the ghost server but cannot be read with confidence is
	// not the table we manage. Appending a second one would be a duplicate-key
	// document, so leave the file alone and say why.
	if at, text, ambiguous := findCodexAmbiguousGhostHeader(lines, codexMCPServerKey); ambiguous {
		_, _ = fmt.Fprintf(w, "  ! %s has a table header ghost cannot parse (line %d: %s)\n", path, at, text)
		_, _ = fmt.Fprintln(w, "    ghost manages a plain [mcp_servers.ghost] table only, so the file was left unchanged.")
		_, _ = fmt.Fprintln(w, "    Rewrite the header as [mcp_servers.ghost] and re-run, or register the server yourself.")
		return false, nil
	}

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

	var out []string
	if found {
		// In place: every line outside the ghost table's own key span stays,
		// and only that span is swapped for the repaired one.
		out = append(out, lines[:start]...)
		out = append(out, repairCodexMCPServerTable(lines, start, end, ghostBin)...)
		out = append(out, lines[end:]...)
	} else {
		// Append at the end, separated by one blank line. Only newlines are
		// added here; existing ones are content and stay put.
		out = append(out, lines[:start]...)
		for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
			out = out[:len(out)-1]
		}
		if len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, codexMCPServerBlockLines(ghostBin)...)
		out = append(out, "") // the registered file ends with a newline
	}

	if err := writeFileAtomic(path, []byte(strings.Join(out, "\n")), 0644); err != nil {
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
		kept := make([]json.RawMessage, 0, len(rules))
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

	out := make(map[string]json.RawMessage, len(top))
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
	if at, text, dotted := findCodexDottedGhost(lines, codexMCPServerKey); dotted {
		return false, fmt.Sprintf("ghost MCP server defined by a dotted or inline key in config.toml (line %d: %s), a form ghost cannot manage", at, text)
	}
	if at, text, ambiguous := findCodexAmbiguousGhostHeader(lines, codexMCPServerKey); ambiguous {
		return false, fmt.Sprintf("ghost MCP server behind a table header ghost cannot parse in config.toml (line %d: %s)", at, text)
	}
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
				ev.Key+" hook missing or miswired (run ghost mcp init --client codex)")
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
