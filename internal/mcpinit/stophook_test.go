package mcpinit

import (
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
