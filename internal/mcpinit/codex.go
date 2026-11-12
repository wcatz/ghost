package mcpinit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/hostevent"
	"github.com/wcatz/ghost/internal/memory"
)

const codexSessionEndTimeoutSec = 3

const codexMCPServerKey = "mcp_servers.ghost"

const codexHooksDescription = "Ghost persistent memory lifecycle hooks (managed by `ghost mcp init --client codex`)"

type codexHookAction struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

type codexHookRule struct {
	Hooks []codexHookAction `json:"hooks"`
}

var codexLifecycleEvents = []struct {
	Key        string
	EventToken string
}{
	{"SessionStart", string(hostevent.EventSessionStart)},
	{"Stop", string(hostevent.EventStop)},
	{"SessionEnd", string(hostevent.EventSessionEnd)},
}

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

func codexTOMLString(s string) string {
	if !strings.ContainsAny(s, "'\n\r") {
		return "'" + s + "'"
	}
	esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`)
	return `"` + esc.Replace(s) + `"`
}

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

func codexValueComplete(value string) bool {
	complete, _ := codexScanValue(value)
	return complete
}

func codexMultilineStillOpen(value string, rest []string) bool {
	delim := codexOpenMultilineDelimiter(value)
	if delim == "" {
		return false
	}
	for _, line := range rest {
		if strings.Contains(line, delim) {
			return true
		}
	}
	return false
}

func codexOpenMultilineDelimiter(value string) string {
	_, delim := codexScanValue(value)
	return delim
}

func codexScanValue(value string) (complete bool, openDelim string) {
	depth := 0
	for i := 0; i < len(value); i++ {
		switch c := value[i]; c {
		case '#':
			if depth == 0 {
				return true, ""
			}
			for i < len(value) && value[i] != '\n' {
				i++
			}
		case '\'', '"':
			end, open, delim := codexStringEndOpen(value, i)
			if open {
				return false, delim
			}
			i = end
		case '[', '{':
			depth++
		case ']', '}':
			depth--
			if depth < 0 {
				return true, ""
			}
		}
	}
	return depth == 0, ""
}

func codexStringEnd(value string, i int) int {
	end, _, _ := codexStringEndOpen(value, i)
	return end
}

func codexStringEndOpen(value string, i int) (end int, open bool, delim string) {
	quote := value[i]
	if i+3 <= len(value) && value[i+1] == quote && value[i+2] == quote {
		triple := value[i : i+3]
		for j := i + 3; j+3 <= len(value); j++ {
			if value[j] != quote {
				continue
			}
			if quote == '"' && j > i+2 && value[j-1] == '\\' {
				continue
			}
			if value[j] == quote && value[j+1] == quote && value[j+2] == quote {
				return j + 2, false, ""
			}
		}
		return len(value) - 1, true, triple
	}
	for i++; i < len(value); i++ {
		switch value[i] {
		case quote:
			return i, false, ""
		case '\\':
			if quote == '"' {
				i++
			}
		}
	}
	return len(value) - 1, false, ""
}

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
	parts := codexDottedParts(inner)
	names := make([]string, 0, len(parts))
	for _, part := range parts {
		if isCodexQuotedToken(part) {

			if strings.ContainsRune(part[1:len(part)-1], '.') {
				return "", false
			}
			part = part[1 : len(part)-1]
		}
		if part == "" || strings.ContainsAny(part, "[]") {
			return "", false
		}
		names = append(names, part)
	}
	return strings.Join(names, "."), true
}

func codexIsTableHeader(line string) bool {
	trimmed := codexStripComment(strings.TrimSpace(line))
	if !strings.HasPrefix(trimmed, "[") || !strings.HasSuffix(trimmed, "]") {
		return false
	}
	return !strings.Contains(trimmed, "=")
}

func codexTableName(line string) string {
	t := codexStripComment(strings.TrimSpace(line))
	t = strings.TrimPrefix(t, "[[")
	t = strings.TrimPrefix(t, "[")
	if end := strings.Index(t, "]"); end >= 0 {
		t = t[:end]
	}
	return strings.TrimSpace(t)
}

func codexValueContinuationLines(lines []string) map[int]bool {
	continuation := make(map[int]bool)
	value, open := "", false
	for i, line := range lines {
		if open && (codexMultilineStillOpen(value, lines[i:]) || !codexHeaderClosesValue(line)) {
			continuation[i] = true
			value += "\n" + line
			if codexValueComplete(value) {
				open, value = false, ""
			}
			continue
		}
		open, value = false, ""
		if codexIsTableHeader(line) {
			continue
		}
		if _, v, ok := splitCodexAssignment(line); ok && !codexValueComplete(v) {
			open, value = true, v
		}
	}
	return continuation
}

func codexHeaderClosesValue(line string) bool {
	return codexBracketedKind(line) == codexTableKey
}

type codexBracketed int

const (
	codexNotAKey codexBracketed = iota

	codexArrayElement

	codexTableKey
)

func codexBracketedKind(line string) codexBracketed {
	if !codexIsTableHeader(line) {
		return codexNotAKey
	}
	t := codexStripComment(strings.TrimSpace(line))
	if strings.HasPrefix(t, "[[") {

		return codexTableKey
	}
	inner := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(t, "["), "]"))
	parts := codexDottedParts(inner)
	quoted := 0
	for _, part := range parts {
		switch {
		case isCodexBareKey(part):
		case isCodexQuotedToken(part):
			quoted++
			if strings.ContainsRune(part[1:len(part)-1], '.') {
				return codexTableKey
			}
		default:
			return codexNotAKey
		}
	}
	switch {
	case len(parts) > 1:
		return codexTableKey
	case quoted > 0:
		return codexArrayElement
	case isCodexBareValue(parts[0]):
		return codexArrayElement
	default:
		return codexTableKey
	}
}

func codexDottedParts(text string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '\'', '"':
			i = codexStringEnd(text, i)
		case '.':
			parts = append(parts, strings.TrimSpace(text[start:i]))
			start = i + 1
		}
	}
	return append(parts, strings.TrimSpace(text[start:]))
}

func isCodexBareKey(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isCodexIdentByte(s[i]) {
			return false
		}
	}
	return true
}

func isCodexQuotedToken(s string) bool {
	if len(s) < 2 || (s[0] != '\'' && s[0] != '"') {
		return false
	}
	return codexStringEnd(s, 0) == len(s)-1
}

func isCodexBareValue(s string) bool {
	switch s {
	case "true", "false":
		return true
	}
	if s == "" {
		return false
	}

	digits := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= '0' && c <= '9':
			digits = true
		case c == '+' || c == '-' || c == '.' || c == ':' || c == 'T' || c == 'Z' ||
			c == 'e' || c == 'E' || c == '_':
		default:
			return false
		}
	}
	return digits
}

func codexAssignmentKeyPath(assigned string) (path string, isPath bool) {
	parts := codexDottedParts(assigned)
	if len(parts) == 1 && isCodexQuotedToken(parts[0]) {
		if strings.ContainsRune(parts[0][1:len(parts[0])-1], '.') {
			return "", false
		}
		return parts[0][1 : len(parts[0])-1], true
	}
	names := make([]string, 0, len(parts))
	for _, part := range parts {
		if isCodexQuotedToken(part) {
			part = part[1 : len(part)-1]
		}
		if part == "" {
			return "", false
		}
		names = append(names, part)
	}
	return strings.Join(names, "."), true
}

const codexBOM = "\ufeff"

func splitCodexLines(body string) (lines []string, bom string) {
	if strings.HasPrefix(body, codexBOM) {
		bom = codexBOM
		body = body[len(codexBOM):]
	}
	return strings.Split(body, "\n"), bom
}

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

func findCodexAmbiguousGhostHeader(lines []string, key string) (at int, text string, ok bool) {
	leaf := key
	if i := strings.LastIndex(key, "."); i >= 0 {
		leaf = key[i+1:]
	}
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "[") {
			continue
		}
		if _, named := normaliseCodexTableName(line); named {
			continue
		}

		if !codexIsTableHeader(line) && !codexHeaderCandidate(line) {
			continue
		}
		if codexHeaderNamesPart(line, leaf) {
			return i + 1, trimmed, true
		}
	}
	return 0, "", false
}

func codexHeaderCandidate(line string) bool {
	t := codexStripComment(strings.TrimSpace(line))
	if !strings.HasPrefix(t, "[") {
		return false
	}
	first := codexDottedParts(strings.TrimLeft(t, "["))
	if len(first) == 0 {
		return false
	}
	return isCodexBareKey(first[0]) || isCodexQuotedToken(first[0])
}

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
			continue
		}
		if i+len(part) < len(t) && isCodexIdentByte(t[i+len(part)]) {
			continue
		}
		return true
	}
	return false
}

func splitCodexAssignment(line string) (key, value string, ok bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", "", false
	}
	eq := strings.Index(trimmed, "=")
	if eq < 0 {
		return "", "", false
	}
	return strings.TrimSpace(trimmed[:eq]), strings.TrimSpace(trimmed[eq+1:]), true
}

func codexOwnedKeyName(key string) string {
	if len(key) > 0 && (key[0] == 0x27 || key[0] == 0x22) && codexStringEnd(key, 0) == len(key)-1 {
		return key[1 : len(key)-1]
	}
	return key
}

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
				table = codexTableName(line)
			}
			continue
		}
		assigned, value, isAssign := splitCodexAssignment(line)
		if !isAssign {
			continue
		}
		assignedPath, isPath := codexAssignmentKeyPath(assigned)
		if !isPath {
			continue
		}
		full := assignedPath
		if table != "" {
			full = table + "." + assignedPath
		}
		if (full == key || strings.HasPrefix(full, key+".")) && !codexTableWithin(table, key) {
			return i + 1, strings.TrimSpace(line), true
		}

		if full == "mcp_servers" && codexInlineNamesKey(codexValueText(value, lines, i), "ghost") {
			return i + 1, strings.TrimSpace(line), true
		}
	}
	return 0, "", false
}

func codexValueText(value string, lines []string, i int) string {
	if codexValueComplete(value) {
		return value
	}
	return strings.Join(lines[i:codexValueLastLine(value, lines, i, len(lines))+1], " ")
}

func codexTableWithin(table, key string) bool {
	return table == key || strings.HasPrefix(table, key+".")
}

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

func isCodexIdentByte(c byte) bool {
	return c == '_' || c == '-' ||
		(c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

type codexMCPServerValues struct {
	Command string
	Args    []string
}

func parseCodexMCPServerBlock(lines []string) codexMCPServerValues {
	var vals codexMCPServerValues
	for _, line := range lines[1:] {
		key, value, ok := splitCodexAssignment(line)
		if !ok {
			continue
		}
		switch codexOwnedKeyName(key) {
		case "command":
			vals.Command = decodeCodexTOMLString(value)
		case "args":
			vals.Args = parseCodexTOMLStringArray(value)
		}
	}
	return vals
}

func codexMCPCurrent(vals codexMCPServerValues, ghostBin string) bool {
	return vals.Command == ghostBin && len(vals.Args) == 1 && vals.Args[0] == "mcp"
}

func codexConfigHoldsOnlyGhostOutput(lines []string) bool {
	start, end, found := findCodexTOMLTable(lines, codexMCPServerKey)
	if !found {
		return false
	}
	for i := 0; i < start; i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" || (i == start-1 && trimmed == codexMCPServerComment) {
			continue
		}
		return false
	}
	for i := end; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "" {
			return false
		}
	}
	for i := start + 1; i < end; i++ {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		key, _, ok := splitCodexAssignment(lines[i])
		if !ok {
			return false
		}
		switch codexOwnedKeyName(key) {
		case "command", "args":
		default:
			return false
		}
	}
	vals := parseCodexMCPServerBlock(lines[start:end])
	return isGhostBinaryName(vals.Command) && len(vals.Args) == 1 && vals.Args[0] == "mcp"
}

const codexMCPServerComment = "# Ghost persistent memory (managed by `ghost mcp init --client codex`)"

type codexOwnedKey struct {
	key  string
	line string
}

func codexMCPServerKeys(ghostBin string) []codexOwnedKey {
	return []codexOwnedKey{
		{"command", "command = " + codexTOMLString(ghostBin) + "\n"},
		{"args", "args = [\"mcp\"]\n"},
	}
}

func codexMCPServerBlockLines(ghostBin string) []string {
	lines := []string{codexMCPServerComment, "[" + codexMCPServerKey + "]"}
	for _, k := range codexMCPServerKeys(ghostBin) {
		lines = append(lines, strings.TrimSuffix(k.line, "\n"))
	}
	return lines
}

func renderCodexMCPServerBlock(ghostBin string) string {
	return strings.Join(codexMCPServerBlockLines(ghostBin), "\n") + "\n"
}

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
			defined[codexOwnedKeyName(key)] = true
		}
	}
	if insertAt < 0 {
		insertAt = end
	}
	var missing []string
	for _, k := range owned {
		if !defined[k.key] {
			missing = append(missing, strings.TrimSuffix(k.line, "\n"))
		}
	}

	out := make([]string, 0, end-start+len(missing)+1)
	if start == 0 || strings.TrimSpace(lines[start-1]) != codexMCPServerComment {
		out = append(out, codexMCPServerComment)
	}
	out = append(out, lines[start])
	written := make(map[string]bool, len(owned))
	inserted := false
	for i := start + 1; i < end; i++ {
		if !inserted && i == insertAt {
			out = append(out, missing...)
			inserted = true
		}
		key, value, ok := splitCodexAssignment(lines[i])
		key = codexOwnedKeyName(key)
		line := ownedLine(key)
		if !ok || line == "" {
			out = append(out, lines[i])
			continue
		}
		if written[key] {

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

func installCodexMCP(w io.Writer, ghostBin string, dryRun bool) (bool, error) {
	path, err := codexConfigTomlPath()
	if err != nil {
		return false, err
	}

	existing, err := readHostConfigFile(path)
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
		return false, fmt.Errorf("read %s: %w", path, err)
	}

	eol := "\n"
	crlf := bytes.Contains(existing, []byte("\r\n"))
	if crlf {
		eol = "\r\n"
	}
	lines, bom := splitCodexLines(string(existing))
	if crlf {
		for i, line := range lines {
			lines[i] = strings.TrimSuffix(line, "\r")
		}
	}

	if at, text, dotted := findCodexDottedGhost(lines, codexMCPServerKey); dotted {
		_, _ = fmt.Fprintf(w, "  ! %s defines the ghost MCP server with a dotted or inline key (line %d: %s)\n", path, at, memory.RefusalArg("config line", text))
		_, _ = fmt.Fprintln(w, "    ghost manages the [mcp_servers.ghost] table only, so the file was left unchanged.")
		_, _ = fmt.Fprintln(w, "    Rewrite the entry as a [mcp_servers.ghost] table and re-run, or register the server yourself.")
		return false, nil
	}

	if at, text, ambiguous := findCodexAmbiguousGhostHeader(lines, codexMCPServerKey); ambiguous {
		_, _ = fmt.Fprintf(w, "  ! %s has a table header ghost cannot parse (line %d: %s)\n", path, at, memory.RefusalArg("config line", text))
		_, _ = fmt.Fprintln(w, "    ghost manages a plain [mcp_servers.ghost] table only, so the file was left unchanged.")
		_, _ = fmt.Fprintln(w, "    Rewrite the header as [mcp_servers.ghost] and re-run, or register the server yourself.")
		return false, nil
	}

	start, end, found := findCodexTOMLTable(lines, codexMCPServerKey)
	if !found {

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

		out = append(out, lines[:start]...)
		out = append(out, repairCodexMCPServerTable(lines, start, end, ghostBin)...)
		out = append(out, lines[end:]...)
	} else {

		out = append(out, lines[:start]...)
		for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
			out = out[:len(out)-1]
		}
		if len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, codexMCPServerBlockLines(ghostBin)...)
		out = append(out, "")
	}

	if len(existing) > 0 && !codexConfigHoldsOnlyGhostOutput(lines) {
		if err := writeBackupOnce(path+".bak", existing); err != nil {
			return false, err
		}
	}

	if err := writeFileAtomic(path, []byte(bom+strings.Join(out, eol)), 0644); err != nil {
		return false, fmt.Errorf("write config.toml: %w", err)
	}
	if found {
		_, _ = fmt.Fprintf(w, "  + repaired the ghost MCP server entry in config.toml (%s)\n", path)
	} else {
		_, _ = fmt.Fprintf(w, "  + registered the ghost MCP server in config.toml (%s)\n", path)
	}
	return true, nil
}

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

func marshalCodexJSON(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		panic(fmt.Sprintf("render codex config: %v", err))
	}
	return buf.String()
}

func codexIsGhostHookAction(cmd, eventToken, ghostBin string) bool {
	_, ok := ghostHookTail(cmd, eventToken, ghostBin)
	return ok
}

func codexLeadingGhostHookAction(cmd, eventToken, ghostBin string) bool {
	bin, rest, ok := splitHookCommand(cmd)
	if !ok || (!isGhostBinaryName(bin) && !isBakedGhostBinary(bin, ghostBin)) {
		return false
	}
	fields := strings.Fields(rest)
	return len(fields) >= 2 && fields[0] == "hook" && fields[1] == eventToken
}

func isBakedGhostBinary(bin, ghostBin string) bool {
	if ghostBin == "" {
		return false
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(bin, ghostBin)
	}
	return bin == ghostBin
}

type codexObject struct {
	fields []codexField
}

type codexField struct {
	key   string
	value json.RawMessage
}

func decodeCodexObject(raw json.RawMessage) (codexObject, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return codexObject{}, false
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return codexObject{}, false
	}
	var obj codexObject
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return codexObject{}, false
		}
		key, ok := keyTok.(string)
		if !ok {
			return codexObject{}, false
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return codexObject{}, false
		}
		obj.fields = append(obj.fields, codexField{key: key, value: value})
	}
	if _, err := dec.Token(); err != nil {
		return codexObject{}, false
	}
	return obj, true
}

func (o codexObject) get(key string) json.RawMessage {
	var found json.RawMessage
	for _, f := range o.fields {
		if f.key != key {
			continue
		}
		if found != nil {
			return nil
		}
		found = f.value
	}
	return found
}

func (o *codexObject) set(key string, value json.RawMessage) {
	for i := range o.fields {
		if o.fields[i].key == key {
			o.fields[i].value = value
			return
		}
	}
	o.fields = append(o.fields, codexField{key: key, value: value})
}

func (o *codexObject) remove(key string) {
	kept := make([]codexField, 0, len(o.fields))
	for _, f := range o.fields {
		if f.key != key {
			kept = append(kept, f)
		}
	}
	o.fields = kept
}

func (o codexObject) marshal() (json.RawMessage, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, f := range o.fields {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, err := marshalNoEscape(f.key)
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(f.value)
	}
	buf.WriteByte('}')
	var out bytes.Buffer
	if err := json.Compact(&out, buf.Bytes()); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func codexRuleRendered(eventToken, ghostBin string) json.RawMessage {
	data, err := marshalNoEscape(codexHookRule{Hooks: []codexHookAction{renderCodexHookAction(eventToken, ghostBin)}})
	if err != nil {
		panic(fmt.Sprintf("render codex hook rule: %v", err))
	}
	return data
}

func codexActionCommand(obj codexObject) (string, bool) {
	raw := obj.get("command")
	if raw == nil {
		return "", false
	}
	var cmd string
	if err := json.Unmarshal(raw, &cmd); err != nil {
		return "", false
	}
	return cmd, cmd != ""
}

func decodeCodexHooksConfig(existing []byte) (top, events map[string]json.RawMessage, err error) {
	if err := leadingBOMError(existing, "hooks.json"); err != nil {
		return nil, nil, err
	}
	if err := json.Unmarshal(existing, &top); err != nil {
		return nil, nil, fmt.Errorf("parse hooks.json: %w", err)
	}

	if top == nil {
		return nil, nil, fmt.Errorf("hooks.json is null")
	}

	events = map[string]json.RawMessage{}
	if raw, ok := top["hooks"]; ok {

		if isJSONNull(raw) {
			return nil, nil, fmt.Errorf("hooks.json: hooks is null")
		}
		if err := json.Unmarshal(raw, &events); err != nil {
			return nil, nil, fmt.Errorf("parse hooks.json hooks: %w", err)
		}
	}

	for _, ev := range codexLifecycleEvents {
		if raw, ok := events[ev.Key]; ok {

			if isJSONNull(raw) {
				return nil, nil, fmt.Errorf("hooks.json: hooks[%q] is null", ev.Key)
			}
			var rules []json.RawMessage
			if err := json.Unmarshal(raw, &rules); err != nil {
				return nil, nil, fmt.Errorf("parse hooks.json %s rules: %w", ev.Key, err)
			}
		}
	}
	return top, events, nil
}

func mergeCodexHooksConfig(existing []byte, ghostBin string) (string, error) {
	top, events, err := decodeCodexHooksConfig(existing)
	if err != nil {
		return "", err
	}

	topOrder := topLevelKeyOrder(existing)
	eventOrder := []string{}
	if hooksRaw, ok := top["hooks"]; ok {
		eventOrder = topLevelKeyOrder(hooksRaw)
	}

	for _, ev := range codexLifecycleEvents {
		if !containsString(eventOrder, ev.Key) {
			eventOrder = append(eventOrder, ev.Key)
		}
		var rules []json.RawMessage
		if raw, ok := events[ev.Key]; ok {
			if err := json.Unmarshal(raw, &rules); err != nil {
				return "", fmt.Errorf("parse hooks.json %s rules: %w", ev.Key, err)
			}
		}
		merged, err := mergeCodexEventRules(rules, ev.EventToken, ghostBin)
		if err != nil {
			return "", err
		}
		rendered := marshalRawSliceCompact(merged)
		events[ev.Key] = rendered
	}

	out := make(map[string]json.RawMessage, len(top))
	for k, v := range top {
		out[k] = v
	}
	if _, ok := out["description"]; !ok {
		desc, err := marshalNoEscape(codexHooksDescription)
		if err != nil {
			return "", fmt.Errorf("render description: %w", err)
		}
		out["description"] = desc
		if !containsString(topOrder, "description") {
			topOrder = append([]string{"description"}, topOrder...)
		}
	}
	hooksBytes, err := marshalOrderedRawObjectCompact(events, eventOrder)
	if err != nil {
		return "", fmt.Errorf("render hooks: %w", err)
	}
	out["hooks"] = hooksBytes

	pretty, err := marshalOrderedRawObjectPretty(out, topOrder)
	if err != nil {
		return "", fmt.Errorf("render hooks document: %w", err)
	}
	return string(pretty) + "\n", nil
}

func mergeCodexEventRules(rules []json.RawMessage, eventToken, ghostBin string) ([]json.RawMessage, error) {
	desired := renderCodexHookAction(eventToken, ghostBin)

	hasSourceCodexWrapper := false
	for _, r := range rules {
		ro, ok := decodeCodexObject(r)
		if !ok {
			continue
		}
		var acts []json.RawMessage
		if raw := ro.get("hooks"); raw != nil {
			_ = json.Unmarshal(raw, &acts)
		}
		for _, a := range acts {
			ao, ok := decodeCodexObject(a)
			if !ok {
				continue
			}
			c, ok := codexActionCommand(ao)
			if !ok || !codexIsGhostHookAction(c, eventToken, ghostBin) {
				continue
			}
			if !codexLeadingGhostHookAction(c, eventToken, ghostBin) {

				if codexWrappedActionCarriesSource(c, eventToken, ghostBin) {
					hasSourceCodexWrapper = true
				}
			}
		}
	}

	out := make([]json.RawMessage, 0, len(rules))
	hasGhost := false
	canonicalKept := false
	for _, rule := range rules {
		obj, ok := decodeCodexObject(rule)
		if !ok {
			out = append(out, rule)
			continue
		}
		var actions []json.RawMessage
		if raw := obj.get("hooks"); raw != nil {
			if err := json.Unmarshal(raw, &actions); err != nil {
				out = append(out, rule)
				continue
			}
		}
		kept := make([]json.RawMessage, 0, len(actions))
		rewrote, dropped := 0, 0
		for _, action := range actions {
			actionObj, ok := decodeCodexObject(action)
			if !ok {
				kept = append(kept, action)
				continue
			}
			cmd, ok := codexActionCommand(actionObj)
			if !ok || !codexIsGhostHookAction(cmd, eventToken, ghostBin) {
				kept = append(kept, action)
				continue
			}
			if !codexLeadingGhostHookAction(cmd, eventToken, ghostBin) {

				kept = append(kept, action)
				if codexWrappedActionCarriesSource(cmd, eventToken, ghostBin) {
					hasSourceCodexWrapper = true
					hasGhost = true
				}
				continue
			}
			if hasSourceCodexWrapper {

				dropped++
				hasGhost = true
				continue
			}
			if canonicalKept {
				dropped++
				continue
			}
			fresh, err := marshalNoEscape(desired.Command)
			if err != nil {
				return nil, fmt.Errorf("render %s rule: %w", eventToken, err)
			}
			actionObj.set("command", fresh)

			typed, err := marshalNoEscape(desired.Type)
			if err != nil {
				return nil, fmt.Errorf("render %s rule: %w", eventToken, err)
			}
			actionObj.set("type", typed)
			if desired.Timeout > 0 {
				budget, err := marshalNoEscape(desired.Timeout)
				if err != nil {
					return nil, fmt.Errorf("render %s rule: %w", eventToken, err)
				}
				actionObj.set("timeout", budget)
			} else {

				actionObj.remove("timeout")
			}
			rewritten, err := actionObj.marshal()
			if err != nil {
				return nil, fmt.Errorf("render %s rule: %w", eventToken, err)
			}
			kept = append(kept, rewritten)
			rewrote++
			canonicalKept = true
			hasGhost = true
		}
		if rewrote == 0 && dropped == 0 {
			out = append(out, rule)
			continue
		}
		if len(kept) == 0 {
			continue
		}
		hooks := marshalRawSliceCompact(kept)
		obj.set("hooks", hooks)
		rebuilt, err := obj.marshal()
		if err != nil {
			return nil, fmt.Errorf("render %s rule: %w", eventToken, err)
		}
		out = append(out, rebuilt)
	}
	if !hasGhost {
		out = append(out, codexRuleRendered(eventToken, ghostBin))
	}
	return out, nil
}

func installCodexHooks(w io.Writer, ghostBin string, dryRun bool) (bool, error) {
	path, err := codexHooksPath()
	if err != nil {
		return false, err
	}

	existing, err := readHostConfigFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("read %s: %w", path, err)
	}

	if len(bytes.TrimSpace(existing)) == 0 {
		existing = nil
	}
	if existing == nil {
		if dryRun {
			_, _ = fmt.Fprintf(w, "  ~ would install hooks.json (%s)\n", path)
			return true, nil
		}
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return false, fmt.Errorf("create codex dir: %w", err)
		}
		if err := writeFileAtomic(path, []byte(renderCodexHooksConfig(ghostBin)), 0644); err != nil {
			return false, fmt.Errorf("write hooks.json: %w", err)
		}
		_, _ = fmt.Fprintf(w, "  + installed hooks.json (%s)\n", path)
		return true, nil
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
	if err := writeFileAtomic(path, []byte(desired), 0644); err != nil {
		return false, fmt.Errorf("write hooks.json: %w", err)
	}
	_, _ = fmt.Fprintf(w, "  + updated hooks.json (%s)\n", path)
	return true, nil
}

func loadCodexHooksRules() (map[string][]json.RawMessage, error) {
	path, err := codexHooksPath()
	if err != nil {
		return nil, err
	}
	data, err := readHostConfigFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if err := leadingBOMError(data, "hooks.json"); err != nil {
		return nil, err
	}
	var top *struct {
		Hooks map[string]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("parse hooks.json: %w", err)
	}

	if top == nil {
		return nil, fmt.Errorf("hooks.json is null")
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

func codexWrappedActionCarriesSource(cmd, eventToken, ghostBin string) bool {
	tail, ok := ghostHookTail(cmd, eventToken, ghostBin)
	if !ok {
		return false
	}
	return codexTailNamesSource(tail, "codex")
}

func codexTailNamesSource(tail []string, wantSource string) bool {
	for i := 0; i < len(tail); i++ {
		if codexTokenEndsCommand(tail[i]) {
			return false
		}
		if tail[i] == "--source" && i+1 < len(tail) && tail[i+1] == wantSource {
			return true
		}
		if strings.HasPrefix(tail[i], "--source=") && strings.TrimPrefix(tail[i], "--source=") == wantSource {
			return true
		}
	}
	return false
}

func codexTokenEndsCommand(tok string) bool {
	if shellConnectives[tok] {
		return true
	}
	return strings.ContainsAny(tok, ";&|")
}

func codexContractHookWired(rules []json.RawMessage, eventToken, wantSource, ghostBin string) bool {
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
			tail, ok := ghostHookTail(action.Command, eventToken, ghostBin)
			if !ok {
				continue
			}
			if codexTailNamesSource(tail, wantSource) {
				return true
			}
		}
	}
	return false
}

func validateCodexHooksFile() error {
	path, err := codexHooksPath()
	if err != nil {
		return err
	}
	data, err := readHostConfigFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		if errors.Is(err, errNotRegularFile) {
			return fmt.Errorf("read %s: %w\n  Replace it with a regular file, or delete it, then re-run `ghost mcp init`", path, err)
		}
		return fmt.Errorf("read %s: %w", path, err)
	}

	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if _, _, err := decodeCodexHooksConfig(data); err != nil {
		return fmt.Errorf("%w\n  Fix or delete %s, then re-run `ghost mcp init`", err, path)
	}
	return nil
}

// RunCodex installs Ghost's codex integration: the [mcp_servers.ghost] stdio
// entry merged textually into ~/.codex/config.toml and SessionStart/Stop/

func RunCodex(w io.Writer, dryRun bool) error {
	if dryRun {
		_, _ = fmt.Fprintf(w, "\nDry run — showing what would change:\n\n")
	}

	if err := validateCodexHooksFile(); err != nil {
		return err
	}

	_, _ = fmt.Fprintln(w, "[1/4] Checking prerequisites...")
	ghostBin, _, err := checkPrereqs(w, "codex")
	if err != nil {
		return retryHint(err)
	}

	_, _ = fmt.Fprintln(w, "\n[2/4] Ensuring ghost config file...")
	if err := ensureConfigBootstrap(w, dryRun); err != nil {
		return retryHint(err)
	}

	_, _ = fmt.Fprintln(w, "\n[3/4] Merging ghost MCP server into config.toml...")
	mcpChanged, err := installCodexMCP(w, ghostBin, dryRun)
	if err != nil {
		return stepError(err)
	}

	_, _ = fmt.Fprintln(w, "\n[4/4] Installing lifecycle hooks...")
	hooksChanged, err := installCodexHooks(w, ghostBin, dryRun)
	if err != nil {
		return stepError(err)
	}

	if !dryRun {
		_, _ = fmt.Fprintln(w, "\nRequired one-time step: run /hooks inside codex and approve (trust) the ghost entries — codex silently skips them until trusted.")
		if mcpChanged || hooksChanged {
			_, _ = fmt.Fprintln(w, "Restart codex to activate.")
		}
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

func codexMCPEntryStatus(ghostBin string) (bool, string) {
	path, err := codexConfigTomlPath()
	if err != nil {
		return false, err.Error()
	}
	data, err := readHostConfigFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, "config.toml not found (run ghost mcp init --client codex)"
		}
		return false, fmt.Sprintf("cannot read %s: %v", path, err)
	}

	lines, _ := splitCodexLines(string(data))
	if at, text, dotted := findCodexDottedGhost(lines, codexMCPServerKey); dotted {
		return false, fmt.Sprintf("ghost MCP server defined by a dotted or inline key in config.toml (line %d: %s), a form ghost cannot manage", at, memory.RefusalArg("config line", text))
	}
	if at, text, ambiguous := findCodexAmbiguousGhostHeader(lines, codexMCPServerKey); ambiguous {
		return false, fmt.Sprintf("ghost MCP server behind a table header ghost cannot parse in config.toml (line %d: %s)", at, memory.RefusalArg("config line", text))
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

	ghostBin := findGhostBinary()
	check(ghostBin != "",
		fmt.Sprintf("ghost binary: %s", ghostBin),
		"ghost binary not found in PATH")

	reportConfigFile(w)

	mcpOK, mcpFail := codexMCPEntryStatus(ghostBin)
	check(mcpOK, "ghost MCP server registered in config.toml", mcpFail)

	events, herr := loadCodexHooksRules()
	if herr != nil {
		check(false, "", fmt.Sprintf("cannot read hooks: %v", herr))
	} else {
		allWired := true
		for _, ev := range codexLifecycleEvents {
			wired := codexContractHookWired(events[ev.Key], ev.EventToken, string(hostevent.SourceCodex), ghostBin)
			allWired = allWired && wired
			check(wired,
				ev.Key+" hook configured",
				ev.Key+" hook missing or miswired (run ghost mcp init --client codex)")
		}
		if allWired {
			_, _ = fmt.Fprintln(w, "  - hooks are installed; approve them once via /hooks in codex (untrusted hooks are silently skipped)")
		}
	}

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
