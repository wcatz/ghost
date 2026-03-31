// Package mcpinit configures Claude Code to use Ghost as its memory system.
package mcpinit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ghostPermissions is the canonical list of MCP tool permissions to allow.
var ghostPermissions = []string{
	"mcp__ghost__ghost_decision_record",
	"mcp__ghost__ghost_decisions_list",
	"mcp__ghost__ghost_health",
	"mcp__ghost__ghost_list_projects",
	"mcp__ghost__ghost_memories_list",
	"mcp__ghost__ghost_memory_delete",
	"mcp__ghost__ghost_memory_pin",
	"mcp__ghost__ghost_memory_save",
	"mcp__ghost__ghost_memory_search",
	"mcp__ghost__ghost_project_context",
	"mcp__ghost__ghost_save_global",
	"mcp__ghost__ghost_search_all",
	"mcp__ghost__ghost_task_complete",
	"mcp__ghost__ghost_task_create",
	"mcp__ghost__ghost_task_list",
	"mcp__ghost__ghost_task_update",
}

// hookEntry represents one matcher+hooks pair in settings.json.
type hookEntry struct {
	Matcher string       `json:"matcher"`
	Hooks   []hookAction `json:"hooks"`
}

type hookAction struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

// settingsFile provides safe read-modify-write for ~/.claude/settings.json.
// It preserves all keys it does not understand via json.RawMessage.
type settingsFile struct {
	path string
	raw  map[string]json.RawMessage
}

// settingsPath returns the path to ~/.claude/settings.json.
func settingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

// loadSettings reads and parses the settings file.
// If the file does not exist, returns an empty settings ready to populate.
func loadSettings(path string) (*settingsFile, error) {
	s := &settingsFile{
		path: path,
		raw:  make(map[string]json.RawMessage),
	}

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	if err := json.Unmarshal(data, &s.raw); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return s, nil
}

// save writes the settings back to disk atomically, creating a .bak backup first.
func (s *settingsFile) save() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create dir %s: %w", dir, err)
	}

	// Backup existing file.
	if data, err := os.ReadFile(s.path); err == nil {
		if err := os.WriteFile(s.path+".bak", data, 0600); err != nil {
			return fmt.Errorf("backup %s: %w", s.path+".bak", err)
		}
	}

	out, err := json.MarshalIndent(s.raw, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}
	out = append(out, '\n')

	// Atomic write: temp file + rename.
	tmp, err := os.CreateTemp(dir, ".settings-*.json")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()

	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Chmod(tmpPath, 0600); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename temp file: %w", err)
	}
	return nil
}

// getPermissions extracts the permissions.allow string slice.
func (s *settingsFile) getPermissions() ([]string, error) {
	permsRaw, ok := s.raw["permissions"]
	if !ok {
		return nil, nil
	}
	var perms struct {
		Allow []string `json:"allow"`
	}
	if err := json.Unmarshal(permsRaw, &perms); err != nil {
		return nil, err
	}
	return perms.Allow, nil
}

// addPermissions adds any missing ghost permissions and returns the list of
// newly added entries.
func (s *settingsFile) addPermissions(perms []string) ([]string, error) {
	existing, err := s.getPermissions()
	if err != nil {
		return nil, err
	}

	set := make(map[string]bool, len(existing))
	for _, p := range existing {
		set[p] = true
	}

	var added []string
	for _, p := range perms {
		if !set[p] {
			existing = append(existing, p)
			added = append(added, p)
		}
	}

	if len(added) == 0 {
		return nil, nil
	}

	// Rebuild the permissions object, preserving other fields.
	var permsMap map[string]json.RawMessage
	if raw, ok := s.raw["permissions"]; ok {
		if err := json.Unmarshal(raw, &permsMap); err != nil {
			permsMap = make(map[string]json.RawMessage)
		}
	} else {
		permsMap = make(map[string]json.RawMessage)
	}

	allowJSON, err := json.Marshal(existing)
	if err != nil {
		return nil, err
	}
	permsMap["allow"] = allowJSON

	permsJSON, err := json.Marshal(permsMap)
	if err != nil {
		return nil, err
	}
	s.raw["permissions"] = permsJSON

	return added, nil
}

// hasHook checks if a SessionStart hook containing cmdSubstr already exists.
func (s *settingsFile) hasHook(event, cmdSubstr string) bool {
	hooksRaw, ok := s.raw["hooks"]
	if !ok {
		return false
	}

	var hooks map[string][]hookEntry
	if err := json.Unmarshal(hooksRaw, &hooks); err != nil {
		return false
	}

	for _, entry := range hooks[event] {
		for _, h := range entry.Hooks {
			if strings.Contains(h.Command, cmdSubstr) {
				return true
			}
		}
	}
	return false
}

// addHook adds a hook entry for the given event. Does not clobber existing hooks.
func (s *settingsFile) addHook(event string, entry hookEntry) error {
	var hooks map[string]json.RawMessage
	if raw, ok := s.raw["hooks"]; ok {
		if err := json.Unmarshal(raw, &hooks); err != nil {
			hooks = make(map[string]json.RawMessage)
		}
	} else {
		hooks = make(map[string]json.RawMessage)
	}

	// Get existing entries for this event.
	var entries []hookEntry
	if raw, ok := hooks[event]; ok {
		if err := json.Unmarshal(raw, &entries); err != nil {
			return fmt.Errorf("parse existing %s hooks: %w", event, err)
		}
	}

	entries = append(entries, entry)

	entriesJSON, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	hooks[event] = entriesJSON

	hooksJSON, err := json.Marshal(hooks)
	if err != nil {
		return err
	}
	s.raw["hooks"] = hooksJSON
	return nil
}
