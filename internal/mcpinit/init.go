package mcpinit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/claudeimport"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/scratch"
)

// Run executes the 10-step Claude Code integration setup.
// When dryRun is true, it reports what would change without modifying anything.
func Run(w io.Writer, dryRun bool) error {

	if PluginInstalled() {
		_, _ = fmt.Fprintln(w, "The ghost Claude Code plugin manages this integration — skipping `ghost mcp init` wiring (use `/plugin update` in Claude Code to update it).")
		_, _ = fmt.Fprintln(w, "To switch to init-managed wiring, uninstall the plugin first.")
		return nil
	}

	if dryRun {
		_, _ = fmt.Fprintf(w, "\nDry run — showing what would change:\n\n")
	}

	if err := validateSettingsFile(); err != nil {
		return err
	}

	if _, err := config.DataDirPath(); err != nil {
		return fmt.Errorf("data directory: %w", err)
	}

	_, _ = fmt.Fprintln(w, "[1/10] Checking prerequisites...")
	ghostBin, claudeBin, err := checkPrereqs(w, "claude")
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintln(w, "\n[2/10] Preparing scratch space...")
	ensureScratchSpace(w, dryRun)

	_, _ = fmt.Fprintln(w, "\n[3/10] Ensuring config file...")
	if err := ensureConfigBootstrap(w, dryRun); err != nil {
		return retryHint(err)
	}

	_, _ = fmt.Fprintln(w, "\n[4/10] Registering MCP server...")
	if err := registerMCP(w, ghostBin, claudeBin, dryRun); err != nil {
		return retryHint(err)
	}

	_, _ = fmt.Fprintln(w, "\n[5/10] Adding tool permissions...")
	settingsFile, err := ensurePermissions(w, dryRun)
	if err != nil {
		return stepError(err)
	}

	_, _ = fmt.Fprintln(w, "\n[6/10] Configuring SessionStart hook...")
	if err := ensureHook(w, settingsFile, ghostBin, dryRun); err != nil {
		return retryHint(err)
	}

	_, _ = fmt.Fprintln(w, "\n[7/10] Configuring Stop and working-moment hooks...")
	if err := ensureStopHook(w, settingsFile, ghostBin, dryRun); err != nil {
		return retryHint(err)
	}
	if err := ensureWorkingMomentHooks(w, settingsFile, ghostBin, dryRun); err != nil {
		return retryHint(err)
	}

	_, _ = fmt.Fprintln(w, "\n[8/10] Disabling Claude Code built-in memory...")
	if err := ensureAutoMemoryDisabled(w, settingsFile, dryRun); err != nil {
		return retryHint(err)
	}

	if dryRun {
		_, _ = fmt.Fprintln(w, "\n  (skipping settings write — dry run)")
	} else {
		if err := settingsFile.save(); err != nil {
			return retryHint(fmt.Errorf("save settings: %w", err))
		}
	}

	_, _ = fmt.Fprintln(w, "\n[9/10] Importing Claude Code memories...")
	projects, err := importMemories(w, dryRun)
	if err != nil {
		_, _ = fmt.Fprintf(w, "  ! import error: %v (continuing)\n", err)
	}

	_, _ = fmt.Fprintln(w, "\n[10/10] Writing project memory redirects...")
	writeRedirects(w, projects, dryRun)

	if dryRun {
		_, _ = fmt.Fprintln(w, "\nNo changes made (dry run).")
	} else {
		_, _ = fmt.Fprintln(w, "\nDone! Restart Claude Code to activate.")
	}
	return nil
}

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

func retryHint(err error) error {
	return fmt.Errorf("%w\n  Re-run `ghost mcp init` to retry", err)
}

func stepError(err error) error {
	if errors.Is(err, errNotRegularFile) {
		return err
	}
	return retryHint(err)
}

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
	removed, failed, err := scratch.Reap(24 * time.Hour)
	if err != nil {
		_, _ = fmt.Fprintf(w, "  ! scratch root %s not reaped: %v (continuing)\n", root, err)
		return
	}
	if failed > 0 {
		_, _ = fmt.Fprintf(w, "  ✓ scratch root %s (reaped %d stale dirs, %d could not be removed)\n", root, removed, failed)
		return
	}
	_, _ = fmt.Fprintf(w, "  ✓ scratch root %s (reaped %d stale dirs)\n", root, removed)
}

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

var systemBinDirs = []string{"/opt/homebrew/bin", "/usr/local/bin"}

// FindClientBinary is the lookup `ghost mcp init` auto-detect uses: PATH, then
// the home bin dirs. It never reads the fixed system dirs, which a sandbox

func FindClientBinary(name string) string {
	if c := homeBinaryCandidates(name); len(c) > 0 {
		return c[0]
	}
	return ""
}

var windowsBinExts = []string{".exe", ".cmd", ".bat"}

func commonBinCandidates(name string) []string {
	candidates := []string{name}
	if runtime.GOOS == "windows" {
		for _, ext := range windowsBinExts {
			candidates = append(candidates, name+ext)
		}
	}
	return candidates
}

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

func findBinary(name string) string {
	if c := binaryCandidates(name); len(c) > 0 {
		return c[0]
	}
	return ""
}

func binaryCandidates(name string) []string {
	out := homeBinaryCandidates(name)
	return appendDirHits(out, systemBinDirs, name)
}

func homeBinaryCandidates(name string) []string {
	var out []string
	if p, err := exec.LookPath(name); err == nil {
		out = append(out, p)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return out
	}
	return appendDirHits(out, []string{
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, "go", "bin"),
	}, name)
}

func appendDirHits(out, dirs []string, name string) []string {
	for _, dir := range dirs {
		for _, candidate := range commonBinCandidates(name) {
			p := filepath.Join(dir, candidate)
			if st, err := os.Stat(p); err == nil && commonDirExecutable(p, st) && !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
	}
	return out
}

const ghostProbeTimeout = 5 * time.Second

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

func registerMCP(w io.Writer, ghostBin, claudeBin string, dryRun bool) error {

	out, err := exec.Command(claudeBin, "mcp", "get", "ghost").CombinedOutput()
	currentOutput := string(out)

	var removeErr error

	present, current := false, false
	if err == nil {
		present, current, _ = claudeMCPVerdict(currentOutput, ghostBin)
	}
	alreadyRegistered := present

	if current {
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

	if alreadyRegistered {
		if out, rerr := exec.Command(claudeBin, "mcp", "remove", "-s", "user", "ghost").CombinedOutput(); rerr != nil {
			removeErr = fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), rerr)
		}
	}

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

func rollbackRegistration(claudeBin, previousOutput string, hadPrevious bool, removeErr, base error) error {
	switch {
	case !hadPrevious:

		return base
	case removeErr != nil:

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

func joinNames(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

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

type claudeRegistration struct {
	config  map[string]any
	dropped []string
}

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

func parseJSONObject(value string) (map[string]any, bool) {
	var obj map[string]any
	if err := json.Unmarshal([]byte(value), &obj); err != nil || obj == nil {
		return nil, false
	}
	return obj, true
}

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
		if errors.Is(err, errNotRegularFile) {
			return fmt.Errorf("%w\n  Replace it with a regular file, or delete it, then re-run `ghost mcp init`", err)
		}
		return fmt.Errorf("%w\n  Fix the named field in %s, then re-run `ghost mcp init`", err, path)
	}
	return nil
}

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

func shellQuote(s string) string {
	if runtime.GOOS == "windows" {
		return shellQuoteWindows(s)
	}
	return shellQuotePOSIX(s)
}

func shellQuoteWindows(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func shellQuotePOSIX(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

type hookReconcileAction int

const (
	hookUnchanged hookReconcileAction = iota
	hookAdded
	hookMigrated
)

func reconcileHook(sf *settingsFile, event, cmdSubstr, desiredCmd, legacyCmd string, isWindows bool) (hookReconcileAction, error) {
	return reconcileHookMatcher(sf, event, "", cmdSubstr, desiredCmd, legacyCmd, isWindows)
}

func reconcileHookMatcher(sf *settingsFile, event, matcher, cmdSubstr, desiredCmd, legacyCmd string, isWindows bool) (hookReconcileAction, error) {
	cmds, err := sf.hookCommands(event)
	if err != nil {
		return hookUnchanged, fmt.Errorf("parse existing %s hooks: %w", event, err)
	}

	existingCmd, exists := "", false
	for _, cmd := range cmds {
		if isGhostHookCommand(cmd, cmdSubstr, desiredCmd) {
			existingCmd, exists = cmd, true
			break
		}
	}

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

func isGhostHookCommand(cmd, cmdSubstr, desiredCmd string) bool {
	eventToken := strings.TrimPrefix(cmdSubstr, "hook ")
	if eventToken == cmdSubstr {
		return false
	}
	desiredBin, _, _ := splitHookCommand(desiredCmd)
	_, ok := ghostHookTail(cmd, eventToken, desiredBin)
	return ok
}

func ghostHookTail(cmd, eventToken, ghostBin string) ([]string, bool) {

	tokens, _ := hookCommandTokens(cmd)
	if tail, ok := ghostTailOver(tokens, eventToken, ghostBin, 0); ok {
		return tail, true
	}
	return nil, false
}

var shellConnectives = map[string]bool{
	"exec": true, ";": true, "&&": true, "||": true, "|": true, "&": true,
}

var transparentPrefixes = map[string]bool{
	"sudo": true, "nice": true, "command": true, "env": true,
	"nohup": true, "time": true, "doas": true, "stdbuf": true,
	"ionice": true, "chrt": true, "taskset": true, "timeout": true,
}

var prefixPositionalSlots = map[string]int{
	"timeout": 1, "taskset": 1, "chrt": 1,
}

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

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

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

var shellOptionTakesOperand = map[string]bool{
	"-o": true, "--option": true, "-O": true,
}

func isLikelyOption(w string) bool {
	return len(w) > 1 && strings.HasPrefix(w, "-")
}

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

		if idx > 0 && shellOptionTakesOperand[tokens[idx-1]] {
			idx -= 2
			continue
		}
		return false
	}
	return false
}

func isShellCommandListFlag(word string) bool {
	return len(word) > 1 && strings.HasPrefix(word, "-") && !strings.HasPrefix(word, "--") && strings.HasSuffix(word, "c")
}

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

func isAssignmentWord(word string) bool {
	eq := strings.IndexByte(word, '=')
	return eq > 0
}

func isGhostToken(token, desiredBin string) bool {
	if isGhostBinaryName(token) {
		return true
	}
	return desiredBin != "" && isBakedGhostBinary(token, desiredBin)
}

func hookCommandTokens(cmd string) ([]string, bool) {
	var tokens []string

	cmd = strings.ReplaceAll(cmd, "\r\n", "\n")
	cmd = strings.ReplaceAll(cmd, "\n", " ; ")
	for {

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

func isGhostBinaryName(path string) bool {
	base := strings.ToLower(hookCommandBase(path))
	for _, ext := range []string{".exe", ".bat", ".cmd"} {
		base = strings.TrimSuffix(base, ext)
	}
	return base == "ghost"
}

func hookCommandBase(path string) string {
	i := strings.LastIndexAny(path, `/\`)
	return path[i+1:]
}

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

const workMomentMatcher = "Edit|Write|MultiEdit"

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

func migratePreContractHook(sf *settingsFile, event, bareTail, desiredCmd string) (bool, error) {
	cmds, err := sf.hookCommands(event)
	if err != nil || len(cmds) == 0 {
		return false, err
	}
	migrated := false
	seen := make(map[string]bool, len(cmds))
	for _, cmd := range cmds {
		if seen[cmd] {
			continue
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

func warnPercentPath(w io.Writer, ghostBin string) {
	if runtime.GOOS == "windows" && strings.Contains(ghostBin, "%") {
		_, _ = fmt.Fprintf(w, "  ! warning: ghost binary path %q contains '%%', which cmd.exe may substitute as an environment variable when the hook runs — consider installing ghost to a path without '%%'\n", ghostBin)
	}
}

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

func importMemories(w io.Writer, dryRun bool) ([]projectInfo, error) {

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
			_, _ = fmt.Fprintf(w, "  ~ %s — would scan for importable memories\n", progressLabel(p.Name))
			continue
		}

		n, refused, failed, notRegular, err := claudeimport.Import(ctx, store, p.ID, p.Path, logger)
		if err != nil {
			_, _ = fmt.Fprintf(w, "  ! %s — import error: %v\n", progressLabel(p.Name), err)
			continue
		}
		if n > 0 {
			_, _ = fmt.Fprintf(w, "  ✓ %s — %d memories imported\n", progressLabel(p.Name), n)
		} else {
			_, _ = fmt.Fprintf(w, "  - %s — no new memories\n", progressLabel(p.Name))
		}
		for _, f := range refused {
			_, _ = fmt.Fprintf(w, "  ! %s — refused by the token guard: %s\n", progressLabel(p.Name), memory.ProjectArg("file", f))
		}
		for _, f := range failed {
			_, _ = fmt.Fprintf(w, "  ! %s — could not store (a later run retries it): %s\n", progressLabel(p.Name), memory.ProjectArg("file", f))
		}
		for _, f := range notRegular {
			_, _ = fmt.Fprintf(w, "  ! %s — skipped, not a regular file (never read): %s\n", progressLabel(p.Name), memory.ProjectArg("file", f))
		}
	}

	return infos, nil
}

func progressLabel(name string) string {
	if placeholder, withheld := memory.TokenGuardPlaceholder("project", name); withheld {
		return placeholder
	}
	return assemble.Label(name)
}

func sanitizeName(name string) string {
	if _, withheld := memory.TokenGuardPlaceholder("project", name); withheld {
		return "unknown"
	}
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

		data, err := readHostConfigFile(target)
		if err != nil {

			if !errors.Is(err, os.ErrNotExist) {
				_, _ = fmt.Fprintf(w, "  ! %s — %v\n", progressLabel(p.Name), err)
				continue
			}

		} else {
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
				_, _ = fmt.Fprintf(w, "  ✓ %s — redirect exists\n", progressLabel(p.Name))
				continue
			}

			if content != oldContent {
				_, _ = fmt.Fprintf(w, "  - %s — MEMORY.md exists (not overwriting)\n", progressLabel(p.Name))
				continue
			}

		}

		if dryRun {
			_, _ = fmt.Fprintf(w, "  ~ %s — would create redirect\n", progressLabel(p.Name))
			continue
		}

		if err := os.MkdirAll(dir, 0755); err != nil {
			_, _ = fmt.Fprintf(w, "  ! %s — mkdir error: %v\n", progressLabel(p.Name), err)
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
			_, _ = fmt.Fprintf(w, "  ! %s — write error: %v\n", progressLabel(p.Name), err)
			continue
		}
		_, _ = fmt.Fprintf(w, "  + %s — created redirect\n", progressLabel(p.Name))
	}
}
