package hostevent

import (
	"strings"
	"testing"
)

const (
	lineToolBash  = `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{}}]}}`
	lineGhostSave = `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"mcp__ghost__ghost_memory_save","input":{}}]}}`
	lineGhostGlob = `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"mcp__ghost__ghost_save_global","input":{}}]}}`
	lineText      = `{"type":"assistant","message":{"content":[{"type":"text","text":"mentioning mcp__ghost__ghost_memory_save in prose does not count"}]}}`
	lineUser      = `{"type":"user","message":{"content":[{"type":"text","text":"hello"}]}}`
)

func TestScanClaudeJSONL(t *testing.T) {
	cases := []struct {
		name          string
		lines         []string
		wantToolCalls int
		wantSaves     int
	}{
		{"tools but no saves", []string{lineUser, lineToolBash, lineText}, 1, 0},
		{"save suppresses nudge", []string{lineToolBash, lineGhostSave}, 2, 1},
		{"global save counts", []string{lineToolBash, lineGhostGlob}, 2, 1},
		{"prose mention is not a save", []string{lineToolBash, lineText}, 1, 0},
		{"pure conversation", []string{lineUser, lineText}, 0, 0},
		{"garbage lines skipped", []string{"garbage not json", lineToolBash, "{{{{", lineGhostSave}, 2, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ScanClaudeJSONL(strings.NewReader(strings.Join(tc.lines, "\n") + "\n"))
			if got.ToolCalls != tc.wantToolCalls || got.GhostSaves != tc.wantSaves {
				t.Errorf("ScanClaudeJSONL = %+v, want toolCalls=%d saves=%d", got, tc.wantToolCalls, tc.wantSaves)
			}
		})
	}
}

func TestScanRegistry(t *testing.T) {
	res, ok := Scan(FormatClaudeJSONL, strings.NewReader(lineToolBash))
	if !ok || res.ToolCalls != 1 {
		t.Errorf("Scan(claude-jsonl) = %+v,%v want 1 tool call", res, ok)
	}

	// Unknown formats must report not-ok — callers fail open; scanning is
	// selected by format and never falls back by source.
	if _, ok := Scan(FormatOpencodeMessages, strings.NewReader(lineToolBash)); ok {
		t.Error("Scan(opencode-messages) should be unregistered in v1")
	}
	if _, ok := Scan("", strings.NewReader(lineToolBash)); ok {
		t.Error("Scan(\"\") should be unregistered")
	}
}
