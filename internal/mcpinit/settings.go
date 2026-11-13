// Package mcpinit configures MCP clients (Claude Code, opencode, codex, goose)
// to use Ghost as their memory system.
package mcpinit

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

var ghostPermissions = []string{
	"mcp__ghost__ghost_decision_record",
	"mcp__ghost__ghost_decisions_list",
	"mcp__ghost__ghost_health",
	"mcp__ghost__ghost_link_withdraw",
	"mcp__ghost__ghost_list_projects",
	"mcp__ghost__ghost_memories_list",
	"mcp__ghost__ghost_memory_delete",
	"mcp__ghost__ghost_memory_flag",
	"mcp__ghost__ghost_memory_move",
	"mcp__ghost__ghost_memory_pin",
	"mcp__ghost__ghost_memory_promote",
	"mcp__ghost__ghost_memory_save",
	"mcp__ghost__ghost_memory_search",
	"mcp__ghost__ghost_memory_update",
	"mcp__ghost__ghost_project_context",
	"mcp__ghost__ghost_project_delete",
	"mcp__ghost__ghost_resolve",
	"mcp__ghost__ghost_resolve_mark",
	"mcp__ghost__ghost_save_global",
	"mcp__ghost__ghost_search_all",
	"mcp__ghost__ghost_task_complete",
	"mcp__ghost__ghost_task_create",
	"mcp__ghost__ghost_task_list",
	"mcp__ghost__ghost_task_update",
}

type hookEntry struct {
	Matcher string       `json:"matcher"`
	Hooks   []hookAction `json:"hooks"`
}

type hookAction struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

type settingsFile struct {
	path         string
	raw          map[string]json.RawMessage
	keyOrder     []string
	ghostManaged bool
	bom          string
}

const jsonBOM = "\ufeff"

func leadingBOMError(data []byte, what string) error {
	if !bytes.HasPrefix(data, []byte(jsonBOM)) {
		return nil
	}
	return fmt.Errorf("%s starts with a UTF-8 byte-order mark, which its host does not read as JSON — save the file as UTF-8 without a BOM", what)
}

func settingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

func loadSettings(path string) (*settingsFile, error) {
	s := &settingsFile{
		path: path,
		raw:  make(map[string]json.RawMessage),
	}

	data, err := readHostConfigFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	if bytes.HasPrefix(data, []byte(jsonBOM)) {
		s.bom = jsonBOM
		data = data[len(jsonBOM):]
	}
	if len(bytes.TrimSpace(data)) > 0 {
		s.keyOrder = topLevelKeyOrder(data)
		if err := json.Unmarshal(data, &s.raw); err != nil {
			return nil, fmt.Errorf("%s must be a JSON object", filepath.Base(path))
		}
		if s.raw == nil {
			return nil, fmt.Errorf("%s is null", filepath.Base(path))
		}
	}
	s.ghostManaged = s.holdsOnlyGhostManaged()
	return s, nil
}

func (s *settingsFile) save() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create dir %s: %w", dir, err)
	}

	if data, err := readHostConfigFile(s.path); err == nil && !s.ghostManaged {
		if err := writeBackupOnce(s.path+".bak", data); err != nil {
			return err
		}
	}

	out, err := marshalOrderedRawObjectPretty(s.raw, s.keyOrder)
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}
	out = append(out, '\n')
	out = append([]byte(s.bom), out...)

	return writeFileAtomicPrivate(s.path, out, 0600)
}

func writeBackupOnce(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return fmt.Errorf("backup %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(path) // a half-written backup must not survive for O_EXCL to keep
		return fmt.Errorf("backup %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("backup %s: %w", path, err)
	}
	return nil
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	return writeFileMode(path, data, perm, false)
}

func writeFileAtomicPrivate(path string, data []byte, perm os.FileMode) error {
	return writeFileMode(path, data, perm, true)
}

func writeFileMode(path string, data []byte, perm os.FileMode, private bool) error {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	dir := filepath.Dir(path)
	createMode, finalMode := perm, perm
	replace := false
	if info, err := os.Stat(path); err == nil {
		finalMode = info.Mode().Perm()
		createMode = 0600
		replace = true
	}
	if private {
		createMode &= perm
		finalMode &= perm
	}
	tmp, err := createTempWithMode(dir, filepath.Base(path), createMode)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write temp file: %w", err)
	}
	if replace {
		if err := os.Chmod(tmpPath, finalMode); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
			return fmt.Errorf("chmod temp file: %w", err)
		}
	}
	if err := syncTempFile(tmp); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("flush temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename temp file: %w", err)
	}
	_ = syncParentDir(dir)
	return nil
}

var (
	syncTempFile  = func(f *os.File) error { return f.Sync() }
	syncParentDir = syncDir
)

func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := d.Sync()
	if err := d.Close(); err != nil && syncErr == nil {
		syncErr = err
	}
	return syncErr
}

func createTempWithMode(dir, base string, perm os.FileMode) (*os.File, error) {
	var lastErr error
	for attempt := 0; attempt < 8; attempt++ {
		var buf [8]byte
		if _, err := rand.Read(buf[:]); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(filepath.Join(dir, fmt.Sprintf(".%s-%x.tmp", base, buf[:5])),
			os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}

func (s *settingsFile) getPermissions() ([]string, error) {
	permsRaw, ok := s.raw["permissions"]
	if !ok {
		return nil, nil
	}
	if isJSONNull(permsRaw) {
		return nil, fmt.Errorf("%s: permissions is null", s.base())
	}
	var perms struct {
		Allow json.RawMessage `json:"allow"`
	}
	if err := json.Unmarshal(permsRaw, &perms); err != nil {
		return nil, fmt.Errorf("%s: permissions must be an object", s.base())
	}
	if perms.Allow == nil || isJSONNull(perms.Allow) {
		return nil, nil
	}
	var allow []string
	if err := json.Unmarshal(perms.Allow, &allow); err != nil {
		return nil, fmt.Errorf("%s: permissions.allow must be a list of strings", s.base())
	}
	return allow, nil
}

func (s *settingsFile) addPermissions(perms []string) ([]string, error) {
	allowRaw, permsOrder, err := s.getAllowRaw()
	if err != nil {
		return nil, err
	}

	var existing []string
	if allowRaw != nil {
		if err := json.Unmarshal(allowRaw, &existing); err != nil {
			return nil, fmt.Errorf("%s: permissions.allow must be a list of strings", s.base())
		}
	}

	set := make(map[string]bool, len(existing))
	for _, p := range existing {
		set[p] = true
	}

	var added []string
	var newAllow []json.RawMessage
	if allowRaw != nil {
		var existingRaw []json.RawMessage
		if err := json.Unmarshal(allowRaw, &existingRaw); err != nil {
			return nil, fmt.Errorf("%s: permissions.allow must be a list of strings", s.base())
		}
		newAllow = append(newAllow, existingRaw...)
	}
	for _, p := range perms {
		if !set[p] {
			enc, err := escapeJSONString(p)
			if err != nil {
				return nil, err
			}
			newAllow = append(newAllow, json.RawMessage(enc))
			added = append(added, p)
		}
	}

	if len(added) == 0 {
		return nil, nil
	}

	var permsMap map[string]json.RawMessage
	if raw, ok := s.raw["permissions"]; ok {
		if err := json.Unmarshal(raw, &permsMap); err != nil {
			permsMap = make(map[string]json.RawMessage)
		}
	}
	if permsMap == nil {
		permsMap = make(map[string]json.RawMessage)
	}

	allowJSON := marshalRawSliceCompact(newAllow)
	permsMap["allow"] = allowJSON

	if permsOrder != nil && !containsString(permsOrder, "allow") {
		permsOrder = append(permsOrder, "allow")
	}

	permsJSON, err := marshalOrderedRawObjectCompact(permsMap, permsOrder)
	if err != nil {
		return nil, err
	}
	s.setRaw("permissions", permsJSON)

	return added, nil
}

func (s *settingsFile) getAllowRaw() (json.RawMessage, []string, error) {
	permsRaw, ok := s.raw["permissions"]
	if !ok {
		return nil, nil, nil
	}
	if isJSONNull(permsRaw) {
		return nil, nil, fmt.Errorf("%s: permissions is null", s.base())
	}
	permsOrder := topLevelKeyOrder(permsRaw)
	var perms struct {
		Allow json.RawMessage `json:"allow"`
	}
	if err := json.Unmarshal(permsRaw, &perms); err != nil {
		return nil, nil, fmt.Errorf("%s: permissions must be an object", s.base())
	}
	if perms.Allow == nil || isJSONNull(perms.Allow) {
		return nil, permsOrder, nil
	}
	return perms.Allow, permsOrder, nil
}

func escapeJSONString(s string) (string, error) {
	b, err := marshalNoEscape(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (s *settingsFile) setAutoMemoryEnabled(v bool) (changed bool, err error) {
	desired, err := json.Marshal(v)
	if err != nil {
		return false, err
	}

	if existing, ok := s.raw["autoMemoryEnabled"]; ok {
		if string(existing) == string(desired) {
			return false, nil
		}
	}

	s.setRaw("autoMemoryEnabled", desired)
	return true, nil
}

func (s *settingsFile) getAutoMemoryEnabled() (value, present bool) {
	raw, ok := s.raw["autoMemoryEnabled"]
	if !ok {
		return false, false
	}
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		return false, false
	}
	return v, true
}

func isJSONNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

var settingsReadHookEvents = []string{"SessionStart", "Stop", "UserPromptSubmit", "PostToolUse"}

func (s *settingsFile) base() string {
	return filepath.Base(s.path)
}

func (s *settingsFile) hooksMap() (hooks map[string]json.RawMessage, present bool, err error) {
	raw, ok := s.raw["hooks"]
	if !ok {
		return nil, false, nil
	}
	if isJSONNull(raw) {
		return nil, true, fmt.Errorf("%s: hooks is null", s.base())
	}
	if err := json.Unmarshal(raw, &hooks); err != nil {
		return nil, true, fmt.Errorf("%s: hooks must be an object", s.base())
	}
	return hooks, true, nil
}

func (s *settingsFile) hookEventRaw(event string) (raw json.RawMessage, present bool, err error) {
	hooks, hooksPresent, err := s.hooksMap()
	if err != nil || !hooksPresent {
		return nil, false, err
	}
	raw, ok := hooks[event]
	if !ok {
		return nil, false, nil
	}
	if isJSONNull(raw) {
		return nil, true, fmt.Errorf("%s: hooks[%q] is null", s.base(), event)
	}
	return raw, true, nil
}

func (s *settingsFile) hookEventList(event string) (list []json.RawMessage, present bool, err error) {
	raw, present, err := s.hookEventRaw(event)
	if err != nil || !present {
		return nil, present, err
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, true, fmt.Errorf("%s: hooks[%q] must be a list", s.base(), event)
	}
	return list, true, nil
}

func (s *settingsFile) hookEntries(event string) (entries []hookEntry, present bool, err error) {
	list, present, err := s.hookEventList(event)
	if err != nil || !present {
		return nil, present, err
	}
	entries = make([]hookEntry, 0, len(list))
	for _, elem := range list {
		var entry hookEntry
		if err := json.Unmarshal(elem, &entry); err != nil {
			return nil, true, fmt.Errorf("%s: hooks[%q] must be a list", s.base(), event)
		}
		entries = append(entries, entry)
	}
	return entries, true, nil
}

func (s *settingsFile) validate() error {
	if s.raw == nil {
		return fmt.Errorf("%s is null", s.base())
	}
	if _, err := s.getPermissions(); err != nil {
		return err
	}
	for _, event := range settingsReadHookEvents {
		if _, _, err := s.hookEntries(event); err != nil {
			return err
		}
	}
	return nil
}

func (s *settingsFile) holdsOnlyGhostManaged() bool {
	if s.raw == nil {
		return false
	}
	if len(s.raw) == 0 {
		return true
	}
	for k := range s.raw {
		switch k {
		case "permissions", "hooks", "autoMemoryEnabled":
		default:
			return false
		}
	}

	raw, ok := s.raw["permissions"]
	if !ok {
		return false
	}
	var perms map[string]json.RawMessage
	if json.Unmarshal(raw, &perms) != nil || perms == nil {
		return false
	}
	for k := range perms {
		if k != "allow" {
			return false
		}
	}
	allow, err := s.getPermissions()
	if err != nil || !sameStringSet(allow, ghostPermissions) {
		return false
	}

	hooks, present, err := s.hooksMap()
	if err != nil || !present || len(hooks) == 0 {
		return false
	}
	for event := range hooks {
		var token, matcher string
		switch event {
		case "SessionStart":
			token = "session-start"
		case "Stop":
			token = "stop"
		case "UserPromptSubmit":
			token = "message-submit"
		case "PostToolUse":
			token, matcher = "edit", workMomentMatcher
		default:
			return false
		}
		list, _, err := s.hookEventList(event)
		if err != nil || len(list) != 1 || !ghostWrittenHookEntry(list[0], matcher, token) {
			return false
		}
	}

	v, present := s.getAutoMemoryEnabled()
	return present && !v
}

func ghostWrittenHookEntry(raw json.RawMessage, matcher, token string) bool {
	var entry map[string]json.RawMessage
	if json.Unmarshal(raw, &entry) != nil || len(entry) != 2 {
		return false
	}
	var gotMatcher string
	if m, ok := entry["matcher"]; !ok || json.Unmarshal(m, &gotMatcher) != nil || gotMatcher != matcher {
		return false
	}
	var actions []map[string]json.RawMessage
	if h, ok := entry["hooks"]; !ok || json.Unmarshal(h, &actions) != nil || len(actions) != 1 {
		return false
	}
	action := actions[0]
	if len(action) != 2 {
		return false
	}
	var typ, cmd string
	if t, ok := action["type"]; !ok || json.Unmarshal(t, &typ) != nil || typ != "command" {
		return false
	}
	if c, ok := action["command"]; !ok || json.Unmarshal(c, &cmd) != nil {
		return false
	}
	bin, _, ok := splitHookCommand(cmd)
	if !ok || !isGhostBinaryName(bin) {
		return false
	}
	return cmd == shellQuote(bin)+" hook "+token+" --source claude-code"
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
		if seen[s] < 0 {
			return false
		}
	}
	return true
}

func (s *settingsFile) findHookCommand(event, cmdSubstr string) (cmd string, ok bool, err error) {
	entries, present, err := s.hookEntries(event)
	if err != nil || !present {
		return "", false, err
	}
	for _, entry := range entries {
		for _, h := range entry.Hooks {
			if strings.Contains(h.Command, cmdSubstr) {
				return h.Command, true, nil
			}
		}
	}
	return "", false, nil
}

func (s *settingsFile) hookCommands(event string) ([]string, error) {
	entries, present, err := s.hookEntries(event)
	if err != nil || !present {
		return nil, err
	}
	var cmds []string
	for _, entry := range entries {
		for _, h := range entry.Hooks {
			cmds = append(cmds, h.Command)
		}
	}
	return cmds, nil
}

func (s *settingsFile) hasExactHookCommand(event, cmd string) (bool, error) {
	entries, present, err := s.hookEntries(event)
	if err != nil || !present {
		return false, err
	}
	for _, entry := range entries {
		for _, h := range entry.Hooks {
			if h.Command == cmd {
				return true, nil
			}
		}
	}
	return false, nil
}

func (s *settingsFile) replaceHookCommand(event, oldCmd, newCmd string) (bool, error) {
	hooks, hooksPresent, err := s.hooksMap()
	if err != nil || !hooksPresent {
		return false, err
	}
	entries, present, err := s.hookEventList(event)
	if err != nil || !present {
		return false, err
	}

	found := false
	for i, entryRaw := range entries {
		rewritten, changed, err := replaceEntryCommand(entryRaw, oldCmd, newCmd)
		if err != nil {
			return false, fmt.Errorf("rewrite %s hook: %w", event, err)
		}
		if changed {
			entries[i] = rewritten
			found = true
		}
	}
	if !found {
		return false, nil
	}

	entriesJSON := marshalRawSliceCompact(entries)
	hooks[event] = entriesJSON

	hooksOrder := topLevelKeyOrder(s.raw["hooks"])
	hooksJSON, err := marshalOrderedRawObjectCompact(hooks, hooksOrder)
	if err != nil {
		return false, err
	}
	s.setRaw("hooks", hooksJSON)
	return true, nil
}

func replaceEntryCommand(entryRaw json.RawMessage, oldCmd, newCmd string) (out json.RawMessage, changed bool, err error) {
	obj, ok := decodeCodexObject(entryRaw)
	if !ok {
		return nil, false, nil
	}
	hooksRaw := obj.get("hooks")
	if hooksRaw == nil {
		return nil, false, nil
	}
	var actions []json.RawMessage
	if err := json.Unmarshal(hooksRaw, &actions); err != nil {
		return nil, false, nil
	}
	for i, actionRaw := range actions {
		actionObj, ok := decodeCodexObject(actionRaw)
		if !ok {
			continue
		}
		cmdRaw := actionObj.get("command")
		if cmdRaw == nil {
			continue
		}
		var cmd string
		if err := json.Unmarshal(cmdRaw, &cmd); err != nil || cmd != oldCmd {
			continue
		}
		cmdJSON, err := marshalNoEscape(newCmd)
		if err != nil {
			return nil, false, err
		}
		actionObj.set("command", cmdJSON)
		rewritten, err := actionObj.marshal()
		if err != nil {
			return nil, false, err
		}
		actions[i] = rewritten
		changed = true
	}
	if !changed {
		return nil, false, nil
	}
	hooksJSON := marshalRawSliceCompact(actions)
	obj.set("hooks", hooksJSON)
	rebuilt, err := obj.marshal()
	if err != nil {
		return nil, false, err
	}
	return rebuilt, true, nil
}

func (s *settingsFile) addHook(event string, entry hookEntry) error {
	var hooks map[string]json.RawMessage
	var hooksOrder []string
	if raw, ok := s.raw["hooks"]; ok {
		hooksOrder = topLevelKeyOrder(raw)
		if err := json.Unmarshal(raw, &hooks); err != nil {
			hooks = make(map[string]json.RawMessage)
		}
	}
	if hooks == nil {
		hooks = make(map[string]json.RawMessage)
	}

	var entries []json.RawMessage
	if raw, ok := hooks[event]; ok {
		if err := json.Unmarshal(raw, &entries); err != nil {
			return fmt.Errorf("parse existing %s hooks: %w", event, err)
		}
	}

	entryJSON, err := marshalNoEscape(entry)
	if err != nil {
		return err
	}
	entries = append(entries, entryJSON)

	entriesJSON := marshalRawSliceCompact(entries)

	hooks[event] = entriesJSON
	if !containsString(hooksOrder, event) {
		hooksOrder = append(hooksOrder, event)
	}

	hooksJSON, err := marshalOrderedRawObjectCompact(hooks, hooksOrder)
	if err != nil {
		return err
	}
	s.setRaw("hooks", hooksJSON)
	return nil
}

func (s *settingsFile) setRaw(key string, value json.RawMessage) {
	if s.raw == nil {
		s.raw = make(map[string]json.RawMessage)
	}
	if _, ok := s.raw[key]; !ok {
		s.keyOrder = append(s.keyOrder, key)
	}
	s.raw[key] = value
}

func marshalOrderedRawObjectCompact(raw map[string]json.RawMessage, order []string) ([]byte, error) {
	seen := make(map[string]bool)
	var fields []string
	for _, k := range order {
		if _, ok := raw[k]; ok && !seen[k] {
			fields = append(fields, k)
			seen[k] = true
		}
	}
	var rest []string
	for k := range raw {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	fields = append(fields, rest...)

	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range fields {
		if i > 0 {
			buf.WriteByte(',')
		}
		keyBytes, err := marshalNoEscape(k)
		if err != nil {
			return nil, err
		}
		buf.Write(keyBytes)
		buf.WriteByte(':')
		buf.Write(raw[k])
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func marshalOrderedRawObjectPretty(raw map[string]json.RawMessage, order []string) ([]byte, error) {
	compact, err := marshalOrderedRawObjectCompact(raw, order)
	if err != nil {
		return nil, err
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, compact, "", "  "); err != nil {
		return nil, err
	}
	return pretty.Bytes(), nil
}

func marshalRawSliceCompact(items []json.RawMessage) json.RawMessage {
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, it := range items {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(it)
	}
	buf.WriteByte(']')
	return buf.Bytes()
}

func containsString(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func topLevelKeyOrder(raw json.RawMessage) []string {
	obj, ok := decodeCodexObject(raw)
	if !ok {
		return nil
	}
	seen := make(map[string]bool)
	var out []string
	for _, f := range obj.fields {
		if !seen[f.key] {
			out = append(out, f.key)
			seen[f.key] = true
		}
	}
	return out
}

func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
