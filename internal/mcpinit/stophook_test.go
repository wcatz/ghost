package mcpinit

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTranscript writes lines to a temp transcript file and returns its path.
func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return path
}

// stopInput builds the hook's stdin JSON.
func stopInput(t *testing.T, transcriptPath string, active bool) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"transcript_path":  transcriptPath,
		"stop_hook_active": active,
	})
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	return string(b)
}

const (
	lineToolBash  = `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{}}]}}`
	lineGhostSave = `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"mcp__ghost__ghost_memory_save","input":{}}]}}`
	lineText      = `{"type":"assistant","message":{"content":[{"type":"text","text":"mentioning mcp__ghost__ghost_memory_save in prose does not count"}]}}`
	lineUser      = `{"type":"user","message":{"content":[{"type":"text","text":"hello"}]}}`
)

func runStopHook(t *testing.T, stdin string) string {
	t.Helper()
	var out strings.Builder
	HandleStopHook(strings.NewReader(stdin), &out)
	return out.String()
}

func TestHandleStopHook(t *testing.T) {
	t.Run("blocks when tools ran but nothing saved", func(t *testing.T) {
		path := writeTranscript(t, lineUser, lineToolBash, lineText)
		out := runStopHook(t, stopInput(t, path, false))
		if !strings.Contains(out, `"decision":"block"`) {
			t.Errorf("expected block decision, got %q", out)
		}
		if !strings.Contains(out, "ghost_memory_save") {
			t.Errorf("reason should mention ghost_memory_save, got %q", out)
		}
	})

	t.Run("allows when a ghost save happened", func(t *testing.T) {
		path := writeTranscript(t, lineToolBash, lineGhostSave)
		if out := runStopHook(t, stopInput(t, path, false)); out != "" {
			t.Errorf("expected silence, got %q", out)
		}
	})

	t.Run("allows pure conversation with no tool calls", func(t *testing.T) {
		path := writeTranscript(t, lineUser, lineText)
		if out := runStopHook(t, stopInput(t, path, false)); out != "" {
			t.Errorf("expected silence, got %q", out)
		}
	})

	t.Run("tool name in prose does not count as a save", func(t *testing.T) {
		path := writeTranscript(t, lineToolBash, lineText)
		out := runStopHook(t, stopInput(t, path, false))
		if !strings.Contains(out, `"decision":"block"`) {
			t.Errorf("prose mention must not suppress the nudge, got %q", out)
		}
	})

	t.Run("stop_hook_active short-circuits", func(t *testing.T) {
		path := writeTranscript(t, lineToolBash)
		if out := runStopHook(t, stopInput(t, path, true)); out != "" {
			t.Errorf("expected silence when already active, got %q", out)
		}
	})

	t.Run("fail-open on missing transcript", func(t *testing.T) {
		if out := runStopHook(t, stopInput(t, "/nonexistent/transcript.jsonl", false)); out != "" {
			t.Errorf("expected silence, got %q", out)
		}
	})

	t.Run("fail-open on empty transcript path", func(t *testing.T) {
		if out := runStopHook(t, stopInput(t, "", false)); out != "" {
			t.Errorf("expected silence, got %q", out)
		}
	})

	t.Run("fail-open on garbage stdin", func(t *testing.T) {
		if out := runStopHook(t, "{not json"); out != "" {
			t.Errorf("expected silence, got %q", out)
		}
	})

	t.Run("skips unparseable transcript lines", func(t *testing.T) {
		path := writeTranscript(t, "garbage not json", lineToolBash, "{{{{", lineGhostSave)
		if out := runStopHook(t, stopInput(t, path, false)); out != "" {
			t.Errorf("expected silence (save found despite garbage), got %q", out)
		}
	})
}

func TestHandleStopHook_SpawnResolveIfConfigured_NoOpWhenDisabled(t *testing.T) {
	// spawnResolveIfConfigured must be a silent no-op when
	// reflection.auto_resolve is false (the default) — the common case for
	// every user who hasn't opted in. This is the only spawn-path behavior
	// safely testable without a real ghost.db or a real config file: it
	// confirms HandleStopHook still returns promptly and performs its usual
	// block-decision logic even when CWD is set, proving the new call didn't
	// introduce a hang or a panic on the hot path.
	var buf bytes.Buffer
	input := `{"transcript_path":"","stop_hook_active":false,"cwd":"/tmp/does-not-matter"}`
	HandleStopHook(strings.NewReader(input), &buf)
	if buf.Len() != 0 {
		t.Errorf("expected no output for empty transcript_path, got %q", buf.String())
	}
}

// isolatedHome points HOME/XDG_CONFIG_HOME/XDG_DATA_HOME at fresh temp dirs so
// config.Load and config.DataDir can never see the developer's real
// ~/.config/ghost/config.yaml or ~/.local/share/ghost/ghost.db. Without this,
// TestSpawnResolveIfConfigured_NoOpWhenDisabled would only be hermetic by
// accident of the machine it happens to run on.
func isolatedHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	dataHome := filepath.Join(dir, "data")
	t.Setenv("XDG_DATA_HOME", dataHome)
	return dataHome
}

func TestSpawnResolveIfConfigured_NoOpWhenDisabled(t *testing.T) {
	// With no config file present, reflection.auto_resolve defaults to false
	// (internal/config/config.go's defaults map). spawnResolveIfConfigured
	// must return immediately after that check — before ever calling
	// config.DataDir (which creates ~/.local/share/ghost), let alone touching
	// ghost.db, the pidfile, or resolve.log.
	dataHome := isolatedHome(t)

	spawnResolveIfConfigured("/tmp/does-not-matter")

	if _, err := os.Stat(filepath.Join(dataHome, "ghost")); !os.IsNotExist(err) {
		t.Errorf("expected ghost data dir to never be created when auto_resolve is disabled, stat err = %v", err)
	}
}
