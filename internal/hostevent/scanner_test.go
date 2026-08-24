package hostevent

import (
	"errors"
	"os"
	"path/filepath"
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

// opencode-messages fixtures: one {info, parts} object per line, exactly as
// opencode's session.messages API returns them.
const (
	ocLineBash = `{"info":{"id":"m1","role":"assistant","sessionID":"s"},"parts":[` +
		`{"id":"p1","sessionID":"s","messageID":"m1","type":"step-start"},` +
		`{"id":"p2","sessionID":"s","messageID":"m1","type":"tool","callID":"c1","tool":"bash",` +
		`"state":{"status":"completed","input":{"command":"git status"},"output":"clean"}}]}`
	ocLineSave = `{"info":{"id":"m2","role":"assistant","sessionID":"s"},"parts":[` +
		`{"id":"p3","sessionID":"s","messageID":"m2","type":"tool","callID":"c2","tool":"ghost_ghost_memory_save",` +
		`"state":{"status":"completed","input":{"content":"x"}}}]}`
	ocLineGlobal = `{"info":{"id":"m3","role":"assistant","sessionID":"s"},"parts":[` +
		`{"id":"p4","sessionID":"s","messageID":"m3","type":"tool","callID":"c3","tool":"ghost_save_global",` +
		`"state":{"status":"completed","input":{"content":"y"}}}]}`
	ocLineClaudeStyle = `{"info":{"id":"m4","role":"assistant","sessionID":"s"},"parts":[` +
		`{"id":"p5","sessionID":"s","messageID":"m4","type":"tool","callID":"c4","tool":"mcp__ghost__ghost_memory_save",` +
		`"state":{"status":"completed"}}]}`
	ocLineText = `{"info":{"id":"m5","role":"assistant","sessionID":"s"},"parts":[` +
		`{"id":"p6","sessionID":"s","messageID":"m5","type":"text",` +
		`"text":"mentioning ghost_ghost_memory_save in prose does not count"}]}`
	ocLineUser = `{"info":{"id":"m6","role":"user","sessionID":"s"},"parts":[` +
		`{"id":"p7","sessionID":"s","messageID":"m6","type":"text","text":"hello"}]}`
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
			got, err := ScanClaudeJSONL(strings.NewReader(strings.Join(tc.lines, "\n") + "\n"))
			if err != nil {
				t.Fatalf("ScanClaudeJSONL: %v", err)
			}
			if got.ToolCalls != tc.wantToolCalls || got.GhostSaves != tc.wantSaves {
				t.Errorf("ScanClaudeJSONL = %+v, want toolCalls=%d saves=%d", got, tc.wantToolCalls, tc.wantSaves)
			}
		})
	}
}

// errReader yields some valid lines then fails, simulating a mid-file I/O
// error or scanner abort.
type errReader struct {
	data []byte
	pos  int
}

func (e *errReader) Read(p []byte) (int, error) {
	if e.pos >= len(e.data) {
		return 0, errors.New("read failure mid-transcript")
	}
	n := copy(p, e.data[e.pos:])
	e.pos += n
	return n, nil
}

// TestScanErrorPropagated pins the contract that a partially-read transcript
// surfaces as an error: counts alone would risk blocking a stop whose save
// sat after the cut. The reader delivers one full line, then a partial line
// with no terminator, then fails — so the save line is lost mid-read.
func TestScanErrorPropagated(t *testing.T) {
	data := lineToolBash + "\n" + lineGhostSave[:len(lineGhostSave)-10]
	r := &errReader{data: []byte(data)}
	res, err := ScanClaudeJSONL(r)
	if err == nil {
		t.Fatal("expected scan error from truncated transcript")
	}
	if res.ToolCalls != 1 {
		t.Errorf("partial ToolCalls = %d, want 1 (save after the cut must be missing)", res.ToolCalls)
	}
	if _, _, err := Scan(FormatClaudeJSONL, r); err == nil {
		t.Error("Scan must propagate the scanner error")
	}
}

func TestScanRegistry(t *testing.T) {
	res, ok, err := Scan(FormatClaudeJSONL, strings.NewReader(lineToolBash))
	if !ok || err != nil || res.ToolCalls != 1 {
		t.Errorf("Scan(claude-jsonl) = %+v,%v,%v want 1 tool call", res, ok, err)
	}

	res, ok, err = Scan(FormatOpencodeMessages, strings.NewReader(ocLineBash))
	if !ok || err != nil || res.ToolCalls != 1 {
		t.Errorf("Scan(opencode-messages) = %+v,%v,%v want 1 tool call", res, ok, err)
	}

	// Formats whose adapters have not landed stay unregistered — callers fail
	// open; scanning is selected by format and never falls back by source.
	if _, ok, err := Scan(FormatCodexRollout, strings.NewReader(lineToolBash)); ok || err != nil {
		t.Error("Scan(codex-rollout) should be unregistered until the codex adapter lands")
	}
	if _, ok, err := Scan("", strings.NewReader(lineToolBash)); ok || err != nil {
		t.Error(`Scan("") should be unregistered`)
	}
}

func TestScanOpencodeMessages(t *testing.T) {
	cases := []struct {
		name          string
		lines         []string
		wantToolCalls int
		wantSaves     int
	}{
		{"tools but no saves", []string{ocLineUser, ocLineBash, ocLineText}, 1, 0},
		{"save suppresses nudge", []string{ocLineBash, ocLineSave}, 2, 1},
		{"global save counts", []string{ocLineBash, ocLineGlobal}, 2, 1},
		{"prose mention is not a save", []string{ocLineBash, ocLineText}, 1, 0},
		{"claude-code style name is not a save here", []string{ocLineBash, ocLineClaudeStyle}, 2, 0},
		{"pure conversation", []string{ocLineUser, ocLineText}, 0, 0},
		{"garbage lines skipped", []string{"garbage not json", ocLineBash, "{{{{", ocLineSave}, 2, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ScanOpencodeMessages(strings.NewReader(strings.Join(tc.lines, "\n") + "\n"))
			if err != nil {
				t.Fatalf("ScanOpencodeMessages: %v", err)
			}
			if got.ToolCalls != tc.wantToolCalls || got.GhostSaves != tc.wantSaves {
				t.Errorf("ScanOpencodeMessages = %+v, want toolCalls=%d saves=%d", got, tc.wantToolCalls, tc.wantSaves)
			}
		})
	}
}

// TestScanOpencodeMessages_GoldenFixture pins the scanner against a realistic
// full-fidelity transcript (SDK-shaped info/parts, an erroring tool, non-JSON
// noise, and a tool part on a user-role message that must be ignored).
func TestScanOpencodeMessages_GoldenFixture(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "opencode-messages", "session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck
	got, err := ScanOpencodeMessages(f)
	if err != nil {
		t.Fatalf("ScanOpencodeMessages: %v", err)
	}
	if got.ToolCalls != 4 || got.GhostSaves != 2 {
		t.Errorf("golden fixture = %+v, want toolCalls=4 saves=2", got)
	}
}
