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
	"strings"
)

// ghostPermissions is the canonical list of MCP tool permissions to allow.
var ghostPermissions = []string{
	"mcp__ghost__ghost_decision_record",
	"mcp__ghost__ghost_decisions_list",
	"mcp__ghost__ghost_health",
	"mcp__ghost__ghost_link_withdraw",
	"mcp__ghost__ghost_list_projects",
	"mcp__ghost__ghost_memories_list",
	"mcp__ghost__ghost_memory_delete",
	"mcp__ghost__ghost_memory_flag",
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
	// ghostManaged records whether the document AS LOADED held nothing but
	// what ghost itself writes. It is decided at load, before any step merges
	// into the in-memory document, so it describes the file still on disk when
	// save runs; save reads it to keep the one-time .bak for the user's
	// pre-ghost file instead of capturing ghost's own output.
	ghostManaged bool
	// bom is the leading byte-order mark the file was loaded with, or "".
	// save writes it back first so the user's file keeps its encoding.
	bom string
}

// jsonBOM is the UTF-8 byte-order mark as a string.
const jsonBOM = "\ufeff"

// leadingBOMError refuses a JSON document that starts with a UTF-8 byte-order
// mark, and returns nil for any other document. It serves the readers whose
// host rejects the mark (codex's serde_json, opencode's JSONC reader), where
// encoding/json's own error is an anonymous invalid character: the refusal
// names the mark instead, and the file is never rewritten. what names the file.
func leadingBOMError(data []byte, what string) error {
	if !bytes.HasPrefix(data, []byte(jsonBOM)) {
		return nil
	}
	return fmt.Errorf("%s starts with a UTF-8 byte-order mark, which its host does not read as JSON — save the file as UTF-8 without a BOM", what)
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
// One leading UTF-8 byte-order mark (what a Windows editor or PowerShell 5.1
// `-Encoding UTF8` writes) is stripped before decoding, because Claude Code
// itself reads such a file; save puts it back, so the file keeps its mark.
//
// The read goes through readHostConfigFile, so a path that is not a regular
// file — a FIFO, a device — is refused by name instead of blocking the open
// until a writer appears.
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
	if err := json.Unmarshal(data, &s.raw); err != nil {
		return nil, fmt.Errorf("%s must be a JSON object", filepath.Base(path))
	}
	// A document that is the JSON literal null decodes into a nil map with no
	// error — an empty object leaves the map above in place, so nil means null
	// and nothing else. Refuse it by name rather than hand back a settingsFile
	// whose raw map is nil: the first addPermissions write would panic on
	// assignment to that nil map, mid-way through init's ten steps.
	if s.raw == nil {
		return nil, fmt.Errorf("%s is null", filepath.Base(path))
	}
	s.ghostManaged = s.holdsOnlyGhostManaged()
	return s, nil
}

// save writes the settings back to disk atomically, creating a .bak backup first.
func (s *settingsFile) save() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create dir %s: %w", dir, err)
	}

	// Backup the pre-merge file, once — unless it holds nothing but ghost's own
	// output. The .bak is the user's pre-ghost file, and it is never rolled
	// forward, so capturing ghost's bytes there would fill the slot forever
	// and the user's own file could never be kept: on a fresh install the first
	// run creates settings.json, and it is the SECOND run whose save sees a
	// file to back up — ghost's own output. `ghostManaged` is that case, and
	// only a file with user content of its own is backed up.
	//
	// The read is readHostConfigFile's, for the same reason every other read
	// of this path is: a file that is not a regular file cannot be captured,
	// and reading it could block. A path that cannot be read is simply not
	// backed up, exactly as an unreadable file never was.
	if data, err := readHostConfigFile(s.path); err == nil && !s.ghostManaged {
		if err := writeBackupOnce(s.path+".bak", data); err != nil {
			return err
		}
	}

	out, err := json.MarshalIndent(s.raw, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}
	out = append(out, '\n')
	out = append([]byte(s.bom), out...)

	return writeFileAtomicPrivate(s.path, out, 0600)
}

// writeBackupOnce captures data at path only when nothing is there yet. The
// .bak is meant to hold the user's pre-ghost file, so rolling it forward on
// every save would leave the only pristine copy overwritten by ghost's own
// previous output: a second init would then destroy the original. O_EXCL
// makes the check and the create a single step, so two concurrent saves cannot
// both believe they were first.
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

// writeFileAtomic writes data to path through a temp file in the same
// directory followed by a rename, so a failed or interrupted write can never
// leave a half-written user config behind. Every write to a file the user owns
// (settings.json, codex config.toml) goes through here rather than a bare
// os.WriteFile. A symlinked config (a dotfiles checkout, say) is resolved
// first: renaming over the link itself would replace it with a regular file
// and strand the real config. A link whose target does not exist yet is the
// one case that cannot be resolved, and the rename then replaces the link with
// a regular file holding the new document — the link is gone, and nothing is
// written at the path it pointed at. An existing file keeps its own
// permissions, so rewriting a 0600 config.toml cannot silently widen it; a new
// one is created with perm, which the kernel narrows by the caller's umask
// just as os.WriteFile would.
//
// The rename makes the document atomic for a reader, and the two flushes make
// it durable for a power cut: without them a loss of power between the write
// and the rename can leave a config that is empty or partly written, and one
// after it can leave a directory entry that never reached the disk, so the
// file is gone even though its inode was written. See writeFileMode.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	return writeFileMode(path, data, perm, false)
}

// writeFileAtomicPrivate is writeFileAtomic with a ceiling: the result can never
// be more permissive than perm, whatever mode the target already had. A file
// that can hold credentials uses this one. settings.json carries hook commands
// and env, and every save before the atomic-write refactor chmod'd it to 0600,
// so a user copy left at 0644 by hand or by another tool must not stay readable
// by group or world once ghost merges into it.
func writeFileAtomicPrivate(path string, data []byte, perm os.FileMode) error {
	return writeFileMode(path, data, perm, true)
}

func writeFileMode(path string, data []byte, perm os.FileMode, private bool) error {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	dir := filepath.Dir(path)
	// The two cases differ. A new file is created with the requested mode, so
	// the kernel narrows it by the umask. A replacement is created 0600 and only
	// afterwards given the target's exact mode: that is the shape the
	// settings.json write has always had, and it keeps the write independent of
	// the target's owner-write bit instead of relying on the exemption a
	// just-created inode gets.
	createMode, finalMode := perm, perm
	replace := false
	if info, err := os.Stat(path); err == nil {
		finalMode = info.Mode().Perm()
		createMode = 0600
		replace = true
	}
	if private {
		// Clamp, never raise: a 0400 file stays 0400.
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
	// A created file already carries the mode the kernel gave it; only a
	// replacement needs the target's exact permissions forced onto it. Forced
	// before the flush so the one sync below carries the mode out with the data.
	if replace {
		if err := os.Chmod(tmpPath, finalMode); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
			return fmt.Errorf("chmod temp file: %w", err)
		}
	}
	// The flush is what makes the temp file's content survive a power cut, and
	// it has to happen here: once the rename lands, the file IS the config, and
	// a power cut before its pages are written leaves a config that reads as
	// empty or truncated — the exact outcome the temp file exists to prevent,
	// reached through the rename that was supposed to make it safe. A failure is
	// fatal rather than reported-and-ignored because nothing has been published
	// yet: the user's config is still the one that was there, and saying so is
	// the truth, while renaming anyway would claim a write that cannot be
	// guaranteed to have landed.
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
	// The rename is itself a directory edit, and it reaches the disk no earlier
	// than the directory holding it is flushed. Skipping this leaves a window in
	// which a power cut keeps the file's data and loses the name, so a config
	// the user knows is configured is simply gone. Its failure is deliberately
	// not reported: the rename has already published the document, so there is
	// nothing left to refuse and an error here would fail a write that did
	// happen, on a filesystem that cannot flush a directory at all.
	_ = syncParentDir(dir)
	return nil
}

// syncTempFile and syncParentDir are the two durability steps of the atomic
// write, held as vars because nothing about either is observable from the
// finished result: both are a question of WHEN they ran relative to the rename,
// and the only evidence of that is the call itself. Production wires them to
// the real fsync and nothing else assigns them; a test replaces one to read the
// target and the temp file at that moment, or to fail the flush and see what
// the write published.
var (
	syncTempFile  = func(f *os.File) error { return f.Sync() }
	syncParentDir = syncDir
)

// syncDir flushes a directory's own entries. Windows has no equivalent —
// FlushFileBuffers on a directory handle fails, and NTFS orders the metadata
// the rename changed itself — so it is a no-op there, the same platform split
// shellQuote makes. Failing to open or flush the directory is reported, not
// swallowed; the caller decides what a filesystem that cannot do it means.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	// Closed explicitly rather than deferred, so a close that fails is not
	// dropped on the floor: a handle the kernel could not release is a fact
	// about the flush, and the sync result is the one that matters when both
	// did.
	syncErr := d.Sync()
	if err := d.Close(); err != nil && syncErr == nil {
		syncErr = err
	}
	return syncErr
}

// createTempWithMode opens a uniquely named new file in dir carrying perm, so
// the kernel applies the caller's umask to it. os.CreateTemp would fix the mode
// at 0600 and force a later chmod, which bypasses the umask and would leave a
// new config.toml world-readable on a host with a restrictive umask. The name
// carries random bytes so a leftover file, or one planted in the user's home by
// something else, cannot predict or collide with it; O_EXCL keeps two writers
// from sharing one.
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

// getPermissions extracts the permissions.allow string slice. Absent
// permissions is (nil, nil); a present value the merge cannot read — an
// explicit null, a non-object, a null allow or an allow that is not a list of
// strings — is refused with the file and the field named, so step 5 prints the
// same refusal the preflight does.
func (s *settingsFile) getPermissions() ([]string, error) {
	permsRaw, ok := s.raw["permissions"]
	if !ok {
		return nil, nil
	}
	// An explicit null is present-but-structurally-invalid, not "absent": it
	// decodes into a nil map with no error, and addPermissions would then panic
	// assigning into that nil map. Unlike the hooks site, nothing here may fill
	// the user's declaration in as an empty object, so refuse it by name.
	if isJSONNull(permsRaw) {
		return nil, fmt.Errorf("%s: permissions is null", s.base())
	}
	var perms struct {
		Allow json.RawMessage `json:"allow"`
	}
	if err := json.Unmarshal(permsRaw, &perms); err != nil {
		return nil, fmt.Errorf("%s: permissions must be an object", s.base())
	}
	// An absent allow (and an explicit null allow) is empty, not invalid: the
	// merge has always filled it in with ghost's permissions, and a null there
	// cannot panic the way a null permissions does.
	if perms.Allow == nil || isJSONNull(perms.Allow) {
		return nil, nil
	}
	var allow []string
	if err := json.Unmarshal(perms.Allow, &allow); err != nil {
		return nil, fmt.Errorf("%s: permissions.allow must be a list of strings", s.base())
	}
	return allow, nil
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

	// Rebuild the permissions object, preserving other fields. The nil guard
	// mirrors addHook's: a null value decodes into a nil map with no error, and
	// getPermissions refuses it, but the assignment below must not be able to
	// panic if this is ever reached another way.
	var permsMap map[string]json.RawMessage
	if raw, ok := s.raw["permissions"]; ok {
		if err := json.Unmarshal(raw, &permsMap); err != nil {
			permsMap = make(map[string]json.RawMessage)
		}
	}
	if permsMap == nil {
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

// setAutoMemoryEnabled sets the "autoMemoryEnabled" key in settings.json.
// It returns changed=true when the value was absent or different from v.
// Idempotent: calling it again with the same value is a no-op.
func (s *settingsFile) setAutoMemoryEnabled(v bool) (changed bool, err error) {
	desired, err := json.Marshal(v)
	if err != nil {
		return false, err
	}

	if existing, ok := s.raw["autoMemoryEnabled"]; ok {
		// Already present — compare raw bytes (true vs false).
		if string(existing) == string(desired) {
			return false, nil
		}
	}

	s.raw["autoMemoryEnabled"] = desired
	return true, nil
}

// getAutoMemoryEnabled returns the current value of "autoMemoryEnabled".
// Returns (false, false) when the key is absent (not set).
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

// isJSONNull reports whether raw is the JSON literal null. json.Unmarshal
// happily decodes null into a map or slice as nil with no error, which would
// otherwise make an explicit null indistinguishable from "absent" — that
// distinction matters here because "absent" is safe to fill in via addHook,
// while an explicit null is structurally-invalid-but-present data that must
// halt reconciliation instead.
func isJSONNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

// settingsReadHookEvents are the hook events `ghost mcp init` reads in steps
// 6-8: the SessionStart, Stop and working-moment events it reconciles. Every
// other event is carried through as raw JSON — addHook and replaceHookCommand
// only decode the event they are given — so init must not refuse a file on a
// shape it would have carried as written.
var settingsReadHookEvents = []string{"SessionStart", "Stop", "UserPromptSubmit", "PostToolUse"}

// base is the file name a refusal names.
func (s *settingsFile) base() string {
	return filepath.Base(s.path)
}

// hooksMap returns the parsed "hooks" object. present is false when the key is
// absent, which is safe to fill in; an explicit null or any other non-object is
// refused by name, because addHook and findHookCommand decode it into a map and
// a nil one panics on the next write.
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

// hookEventRaw returns the raw value registered for event. present is false
// when hooks or the event is absent; an explicit null is refused by name.
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

// hookEventList decodes hooks[event] as a JSON array of raw entries, the shape
// the host writes. It is the shared list check behind hookEntries and the
// repair path's replaceHookCommand, so a reader that models entries and one
// that preserves them as raw agree on what "a list" means.
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

// hookEntries returns hooks[event] decoded into entries. present is false when
// hooks or the event is absent. err is non-nil when the value is present but
// structurally invalid — including an explicit JSON null, which decodes into a
// nil slice with no error — so callers must treat that as "stop", not "absent".
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

// validate checks every part of an already-loaded settings.json that
// `ghost mcp init` parses, so a structurally invalid file is refused BEFORE
// step 4 registers the MCP server and the file is left exactly as the user
// wrote it; without it the same document is discovered only after
// registration, or panics on the nil map a JSON null decodes into, leaving
// Claude Code half-wired.
//
// It calls the SAME readers steps 5-8 call (getPermissions and hookEntries),
// so the preflight's refusals and the readers' are one parser and print the
// same messages. Only the events in settingsReadHookEvents are checked: every
// other event is carried through as raw JSON, so a file whose unrelated event
// has an unusual shape still installs.
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

// holdsOnlyGhostManaged reports whether the document as loaded holds nothing
// but what ghost itself writes: ghost's tool permissions, ghost's lifecycle
// hooks, and the autoMemoryEnabled flag, with no other key, permission or
// hook. An empty document counts — it carries nothing of the user's either.
// save reads it to decide whether settings.json is worth backing up, because
// the one-time .bak exists for the user's pre-ghost file: a file that holds
// only ghost's own output has no pre-ghost content to keep, and since the .bak
// is never rolled forward, capturing ghost's bytes there would fill the slot
// for good and the user's own file could never be backed up afterwards.
//
// A hook counts only when it is byte-for-byte what init writes: one entry per
// event holding exactly the keys matcher and hooks, the matcher init uses for
// that event, one action holding exactly the keys type and command, and a
// command equal to the one init builds for the binary it names. This is
// deliberately stricter than the ownership rule the reconcile uses
// (isGhostHookCommand), which also accepts hand-edited wrappers, a timeout or
// a `&& other-tool` tail: those are the user's edits to ghost's hook and the
// file holding them is not ghost's own output. A hook naming an older ghost
// path still counts when it equals what init would have written for that path,
// so an upgrade that moved the binary does not capture the file. Whatever is
// not affirmatively ghost's own makes the answer false, so an unusual shape
// costs a harmless extra backup rather than a skipped one.
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

	// permissions must be exactly ghost's list under an allow-only object:
	// another key (deny, defaultMode) or another permission is the user's.
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

	// hooks must hold only the events ghost writes, each exactly as init
	// writes it (see ghostWrittenHookEntry).
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

// ghostWrittenHookEntry reports whether raw is exactly the entry init writes
// for the event whose hook token is token: the keys matcher and hooks and no
// others, the given matcher, and a single action of exactly type "command" and
// a command equal to shellQuote(binary) + " hook <token> --source claude-code"
// for a binary named ghost. Any other key (timeout, statusMessage), a second
// action, a different matcher or a command that differs by one byte is not
// ghost's own output.
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

// sameStringSet reports whether a and b hold the same strings, ignoring order
// and repeats.
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

// findHookCommand returns the first hook command for event containing
// cmdSubstr, and whether one was found. err is non-nil when the existing
// "hooks" value (or the specific event's value within it) is present but
// structurally invalid, through hookEntries — including explicit JSON null,
// which unmarshals into a nil map/slice with no error — so callers must treat
// that as "stop", not "absent": proceeding to addHook on a nil "hooks" map
// panics, and on a nil per-event value silently discards it.
//
// The match is positional and by substring only, so the command it returns is
// whichever one sorts first, which is another tool's as often as it is
// ghost's. It decides WHICH command an event holds first, never WHOSE it is:
// ownership is the executable test isGhostHookCommand applies, and reading it
// here is what made a foreign command carrying ghost's argument tail count as
// ghost's own hook (#1326).
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

// hookCommands returns every hook command registered for event, preserving
// order. err is non-nil when the "hooks" value (or the event's value) is
// present but structurally invalid, through hookEntries — callers must treat
// that as "stop", mirroring findHookCommand.
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

// hasExactHookCommand reports whether any hook command for event exactly
// equals cmd. Used to find the precise pre-#251 legacy command rather than
// anything that merely contains the same substring — a hand-edited wrapper
// can legitimately contain "hook session-start" too, and must never be
// mistaken for the legacy command.
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

// replaceHookCommand rewrites every hook command for event that exactly
// equals oldCmd to newCmd — an exact match, not a substring one, so a
// hand-edited command that merely contains the same text is never touched.
// Like addHook, it only unmarshals/re-marshals hooks[event], so it does not
// disturb other events or unrelated top-level keys. Returns false if no
// exact match was found.
func (s *settingsFile) replaceHookCommand(event, oldCmd, newCmd string) (bool, error) {
	hooks, hooksPresent, err := s.hooksMap()
	if err != nil || !hooksPresent {
		return false, err
	}
	// Decode only as far as the raw entries: every entry and every hook action
	// stays a raw object, so the fields hookEntry/hookAction do not model
	// (timeout, async, a non-command hook's own payload field, keys the host
	// adds later) and every entry that holds no match are written back exactly
	// as they were. Same policy as mergeCodexHooksConfig.
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

	entriesJSON, err := json.Marshal(entries)
	if err != nil {
		return false, err
	}
	hooks[event] = entriesJSON

	hooksJSON, err := json.Marshal(hooks)
	if err != nil {
		return false, err
	}
	s.raw["hooks"] = hooksJSON
	return true, nil
}

// replaceEntryCommand rewrites, inside one raw hooks[event] entry, every hook
// action whose "command" is exactly oldCmd to newCmd. Only that one string is
// replaced: the entry's other keys and the action's other keys are kept, and
// an entry or action that is not an object, has no "hooks" array or carries no
// string "command" is left untouched rather than reshaped. changed is false
// (and out is nil) when nothing in the entry matched.
func replaceEntryCommand(entryRaw json.RawMessage, oldCmd, newCmd string) (out json.RawMessage, changed bool, err error) {
	var entry map[string]json.RawMessage
	if json.Unmarshal(entryRaw, &entry) != nil || entry == nil {
		return nil, false, nil
	}
	var actions []json.RawMessage
	if json.Unmarshal(entry["hooks"], &actions) != nil {
		return nil, false, nil
	}
	for j, actionRaw := range actions {
		var action map[string]json.RawMessage
		if json.Unmarshal(actionRaw, &action) != nil || action == nil {
			continue
		}
		var cmd string
		if json.Unmarshal(action["command"], &cmd) != nil || cmd != oldCmd {
			continue
		}
		cmdJSON, err := json.Marshal(newCmd)
		if err != nil {
			return nil, false, err
		}
		action["command"] = cmdJSON
		if actions[j], err = json.Marshal(action); err != nil {
			return nil, false, err
		}
		changed = true
	}
	if !changed {
		return nil, false, nil
	}
	if entry["hooks"], err = json.Marshal(actions); err != nil {
		return nil, false, err
	}
	if out, err = json.Marshal(entry); err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// addHook adds a hook entry for the given event. Does not clobber existing hooks.
func (s *settingsFile) addHook(event string, entry hookEntry) error {
	var hooks map[string]json.RawMessage
	if raw, ok := s.raw["hooks"]; ok {
		if err := json.Unmarshal(raw, &hooks); err != nil {
			hooks = make(map[string]json.RawMessage)
		}
	}
	if hooks == nil {
		hooks = make(map[string]json.RawMessage)
	}

	// Get existing entries for this event. They stay raw, so a user entry is
	// carried over whole — every key, not just the ones hookEntry models.
	var entries []json.RawMessage
	if raw, ok := hooks[event]; ok {
		if err := json.Unmarshal(raw, &entries); err != nil {
			return fmt.Errorf("parse existing %s hooks: %w", event, err)
		}
	}

	entryJSON, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	entries = append(entries, entryJSON)

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
