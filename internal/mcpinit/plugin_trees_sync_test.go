package mcpinit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The POSIX plugin/ tree and the Windows plugin-windows/ tree are hand-edited
// copies of each other and must stay identical except for the entry-point
// command (bin/ghost-launcher vs bin/ghost.exe). Drift has happened once
// already: the 2026-08-20 design spec's hook examples omitted
// `--source claude-code`, and only a manual review in PR #442 caught it. That
// omission fails open — the hooks still run, but Ghost cannot attribute them
// to a harness — so the next drift must fail a test, not a human.

// pluginHookAction is one command entry in an event's "hooks" list.
type pluginHookAction struct {
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// pluginHookEvent is one matcher block under a hook event.
type pluginHookEvent struct {
	Hooks []pluginHookAction `json:"hooks"`
}

// pluginHooksDocument is the typed view of hooks.json used for per-event
// pinning. The structural comparison below uses a generic view instead, so
// fields this struct does not name cannot hide in either tree.
type pluginHooksDocument struct {
	Hooks map[string][]pluginHookEvent `json:"hooks"`
}

// readPluginHooks parses one tree's hooks/hooks.json twice: as a typed
// document (for pinning commands and args) and as generic JSON (for the full
// structural comparison).
func readPluginHooks(t *testing.T, rel string) (pluginHooksDocument, any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	var doc pluginHooksDocument
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse %s as typed document: %v", rel, err)
	}
	var generic any
	if err := json.Unmarshal(b, &generic); err != nil {
		t.Fatalf("parse %s as generic JSON: %v", rel, err)
	}
	return doc, generic
}

// normalizeEntryPoints returns a deep copy of v with every string-valued
// "command" field replaced by a shared placeholder — the one difference the
// two trees are allowed to have.
func normalizeEntryPoints(v any) any {
	switch node := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(node))
		for k, val := range node {
			if k == "command" {
				if _, ok := val.(string); ok {
					out[k] = "<entry-point>"
					continue
				}
			}
			out[k] = normalizeEntryPoints(val)
		}
		return out
	case []any:
		out := make([]any, len(node))
		for i, val := range node {
			out[i] = normalizeEntryPoints(val)
		}
		return out
	default:
		return v
	}
}

// diffNormalized line-diffs the two marshaled normalized documents so a
// failure names the exact line that diverged instead of only "not equal".
// json.MarshalIndent sorts map keys, so both renderings are canonical.
func diffNormalized(posixRel, windowsRel string, posix, windows any) string {
	posixRendered, err := json.MarshalIndent(posix, "", "  ")
	if err != nil {
		return fmt.Sprintf("render %s: %v", posixRel, err)
	}
	windowsRendered, err := json.MarshalIndent(windows, "", "  ")
	if err != nil {
		return fmt.Sprintf("render %s: %v", windowsRel, err)
	}
	posixLines := strings.Split(string(posixRendered), "\n")
	windowsLines := strings.Split(string(windowsRendered), "\n")
	var b strings.Builder
	shown := 0
	for i := 0; i < max(len(posixLines), len(windowsLines)) && shown < 5; i++ {
		posixLine, windowsLine := "<missing>", "<missing>"
		if i < len(posixLines) {
			posixLine = posixLines[i]
		}
		if i < len(windowsLines) {
			windowsLine = windowsLines[i]
		}
		if posixLine == windowsLine {
			continue
		}
		fmt.Fprintf(&b, "  normalized line %d:\n    %s: %s\n    %s: %s\n",
			i+1, posixRel, posixLine, windowsRel, windowsLine)
		shown++
	}
	if len(posixLines) != len(windowsLines) {
		fmt.Fprintf(&b, "  normalized line counts differ: %s has %d lines, %s has %d\n",
			posixRel, len(posixLines), windowsRel, len(windowsLines))
	}
	return strings.TrimRight(b.String(), "\n")
}

// TestPluginHookTreesSync asserts the two plugin trees' hooks files stay in
// lockstep. The invariant holds today, so this is a regression guard: it
// fails as soon as either tree is edited without the other.
func TestPluginHookTreesSync(t *testing.T) {
	const (
		posixRel   = "plugin/hooks/hooks.json"
		windowsRel = "plugin-windows/hooks/hooks.json"
	)

	// The entry-point command is the ONLY field allowed to differ; pin each
	// tree's exact value so a typo there fails here, not at hook runtime.
	wantCommand := map[string]string{
		posixRel:   "${CLAUDE_PLUGIN_ROOT}/bin/ghost-launcher",
		windowsRel: "${CLAUDE_PLUGIN_ROOT}/bin/ghost.exe",
	}
	// Both events' args, exactly as they are today. The trailing
	// `--source claude-code` is the fail-open hazard PR #442 caught by
	// hand: without it the hooks still run, but Ghost cannot tell which
	// harness invoked them.
	wantArgs := map[string][]string{
		"SessionStart": {"hook", "session-start", "--source", "claude-code"},
		"Stop":         {"hook", "stop", "--source", "claude-code"},
	}

	posixDoc, posixGeneric := readPluginHooks(t, posixRel)
	windowsDoc, windowsGeneric := readPluginHooks(t, windowsRel)

	// Full structural comparison with only the command fields masked: a new
	// key, a dropped arg, or an edited description in ONE tree fails here,
	// with the diverging line named.
	posixNormalized := normalizeEntryPoints(posixGeneric)
	windowsNormalized := normalizeEntryPoints(windowsGeneric)
	if !reflect.DeepEqual(posixNormalized, windowsNormalized) {
		t.Errorf("%s and %s differ after masking the entry-point command:\n%s",
			posixRel, windowsRel,
			diffNormalized(posixRel, windowsRel, posixNormalized, windowsNormalized))
	}

	docs := map[string]pluginHooksDocument{posixRel: posixDoc, windowsRel: windowsDoc}
	for rel, doc := range docs {
		// An event present in only one tree already fails the structural
		// check above; an event present in both still has to be pinned
		// here, so name unknown events instead of ignoring them.
		for event := range doc.Hooks {
			if _, ok := wantArgs[event]; !ok {
				t.Errorf("%s declares unpinned hook event %q — pin its command and args here once both trees carry it",
					rel, event)
			}
		}
		for event, want := range wantArgs {
			entries, ok := doc.Hooks[event]
			if !ok {
				t.Errorf("%s declares no %s hook", rel, event)
				continue
			}
			if len(entries) != 1 {
				t.Errorf("%s %s: want exactly 1 matcher entry, got %d", rel, event, len(entries))
				continue
			}
			if len(entries[0].Hooks) != 1 {
				t.Errorf("%s %s: want exactly 1 hook action, got %d", rel, event, len(entries[0].Hooks))
				continue
			}
			action := entries[0].Hooks[0]
			if action.Type != "command" {
				t.Errorf("%s %s hook type = %q, want %q", rel, event, action.Type, "command")
			}
			if action.Command != wantCommand[rel] {
				t.Errorf("%s %s command = %q, want exactly %q", rel, event, action.Command, wantCommand[rel])
			}
			if !slices.Equal(action.Args, want) {
				t.Errorf("%s %s args = %q, want exactly %q (the --source claude-code tail is the fail-open hazard PR #442 caught by hand)",
					rel, event, action.Args, want)
			}
		}
	}
}
