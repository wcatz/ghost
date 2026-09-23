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
	if _, _, err := Scan(FormatClaudeJSONL, &errReader{data: []byte(data)}); err == nil {
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

	res, ok, err = Scan(FormatCodexRollout, strings.NewReader(`{"timestamp":"t","type":"response_item","payload":{"type":"function_call","name":"shell"}}`))
	if !ok || err != nil || res.ToolCalls != 1 {
		t.Errorf("Scan(codex-rollout) = %+v,%v,%v want 1 tool call", res, ok, err)
	}

	// Unknown formats stay unregistered — callers fail open; scanning is
	// selected by format and never falls back by source.
	if _, ok, err := Scan("", strings.NewReader(lineToolBash)); ok || err != nil {
		t.Error(`Scan("") should be unregistered`)
	}
}

// codex-rollout fixtures: timestamped envelopes whose payload carries the
// Responses-API items (function_call with optional mcp__<server> namespace,
// local_shell_call for the built-in shell).
const (
	cxMeta     = `{"timestamp":"t0","type":"session_meta","payload":{"id":"s"}}`
	cxMsg      = `{"timestamp":"t1","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"text","text":"ghost_memory_save in prose does not count"}]}}`
	cxShell    = `{"timestamp":"t2","type":"response_item","payload":{"type":"local_shell_call","call_id":"c1","status":"completed","action":{"command":["ls"]}}}`
	cxSaveNS   = `{"timestamp":"t3","type":"response_item","payload":{"type":"function_call","name":"ghost_memory_save","namespace":"mcp__ghost","arguments":"{}","call_id":"c2"}}`
	cxSaveFlat = `{"timestamp":"t4","type":"response_item","payload":{"type":"function_call","name":"ghost_save_global","arguments":"{}","call_id":"c3"}}`
	cxOtherMCP = `{"timestamp":"t5","type":"response_item","payload":{"type":"function_call","name":"search","namespace":"mcp__other","arguments":"{}","call_id":"c4"}}`
	cxEventMsg = `{"timestamp":"t6","type":"event_msg","payload":{"type":"token_count"}}`
)

func TestScanCodexRollout(t *testing.T) {
	cases := []struct {
		name          string
		lines         []string
		wantToolCalls int
		wantSaves     int
	}{
		{"shell counts as a tool call", []string{cxMeta, cxShell}, 1, 0},
		{"namespaced save counts", []string{cxShell, cxSaveNS}, 2, 1},
		{"legacy flat save counts", []string{cxShell, cxSaveFlat}, 2, 1},
		{"another server's tool is not a save", []string{cxShell, cxOtherMCP}, 2, 0},
		{"identically-named tool on another server is not a save", []string{cxShell, `{"timestamp":"t","type":"response_item","payload":{"type":"function_call","name":"ghost_memory_save","namespace":"mcp__other","arguments":"{}","call_id":"c9"}}`}, 2, 0},
		{"non-response lines skipped", []string{cxMeta, cxShell, cxEventMsg}, 1, 0},
		{"garbage skipped", []string{"not json", cxShell, "{{{"}, 1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ScanCodexRollout(strings.NewReader(strings.Join(tc.lines, "\n") + "\n"))
			if err != nil {
				t.Fatalf("ScanCodexRollout: %v", err)
			}
			if got.ToolCalls != tc.wantToolCalls || got.GhostSaves != tc.wantSaves {
				t.Errorf("= %+v, want calls=%d saves=%d", got, tc.wantToolCalls, tc.wantSaves)
			}
		})
	}
}

// TestScanCodexRollout_GoldenFixture pins the scanner against a realistic
// full-fidelity rollout (meta + event noise, shell + MCP function calls,
// non-JSON junk).
func TestScanCodexRollout_GoldenFixture(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "codex-rollout", "session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck
	got, err := ScanCodexRollout(f)
	if err != nil {
		t.Fatalf("ScanCodexRollout: %v", err)
	}
	if got.ToolCalls != 4 || got.GhostSaves != 1 {
		t.Errorf("golden fixture = %+v, want toolCalls=4 saves=1", got)
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

// opencode-v2-messages fixtures: one V2 session.context message per line.
// V2 exposes MCP servers through Code Mode by default, so a ghost save is an
// `execute` tool whose metadata.toolCalls names `ghost.<tool>`; with
// codemode disabled the tool stays native as `ghost_<tool>`.
const (
	oc2LineShell = `{"id":"m1","type":"assistant","agent":"build","content":[` +
		`{"type":"tool","id":"c1","name":"shell","state":{"status":"completed","input":{"command":"git status"}}}]}`
	oc2LineCodeModeSave = `{"id":"m2","type":"assistant","agent":"build","content":[` +
		`{"type":"tool","id":"c2","name":"execute","state":{"status":"completed",` +
		`"input":{"code":"return await tools.ghost.ghost_memory_save({})"},` +
		`"metadata":{"toolCalls":[{"tool":"ghost.ghost_memory_save","status":"completed"}]}}}]}`
	oc2LineCodeModeGlobal = `{"id":"m3","type":"assistant","agent":"build","content":[` +
		`{"type":"tool","id":"c3","name":"execute","state":{"status":"completed",` +
		`"metadata":{"toolCalls":[{"tool":"ghost.ghost_task_list"},{"tool":"ghost.ghost_save_global"}]}}}]}`
	oc2LineCodeModeNoSave = `{"id":"m4","type":"assistant","agent":"build","content":[` +
		`{"type":"tool","id":"c4","name":"execute","state":{"status":"completed",` +
		`"input":{"code":"// ghost.ghost_memory_save later\nreturn 1"},"metadata":{"toolCalls":[]}}}]}`
	oc2LineNativeSave = `{"id":"m5","type":"assistant","agent":"build","content":[` +
		`{"type":"tool","id":"c5","name":"ghost_ghost_memory_save","state":{"status":"completed"}}]}`
	oc2LineText = `{"id":"m6","type":"assistant","agent":"build","content":[` +
		`{"type":"text","text":"mentioning tools.ghost.ghost_memory_save in prose does not count"}]}`
	oc2LineUser    = `{"id":"m7","type":"user","text":"hello","files":[]}`
	oc2LineCatalog = `{"id":"m8","type":"system","text":"- tools.ghost.ghost_memory_save({...}) // Save a memory"}`
)

func TestScanOpencodeV2Messages(t *testing.T) {
	cases := []struct {
		name          string
		lines         []string
		wantToolCalls int
		wantSaves     int
	}{
		{"tools but no saves", []string{oc2LineUser, oc2LineShell, oc2LineText}, 1, 0},
		{"code mode save counts", []string{oc2LineShell, oc2LineCodeModeSave}, 2, 1},
		{"code mode global save among other calls", []string{oc2LineCodeModeGlobal}, 1, 1},
		{"code mentioning the tool without calling it is not a save", []string{oc2LineCodeModeNoSave}, 1, 0},
		{"native (codemode off) save counts", []string{oc2LineShell, oc2LineNativeSave}, 2, 1},
		{"prose and catalog mentions are not saves", []string{oc2LineCatalog, oc2LineShell, oc2LineText}, 1, 0},
		{"v1-shaped line yields nothing", []string{ocLineSave}, 0, 0},
		{"garbage lines skipped", []string{"garbage not json", oc2LineShell, "{{{{", oc2LineCodeModeSave}, 2, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ScanOpencodeV2Messages(strings.NewReader(strings.Join(tc.lines, "\n") + "\n"))
			if err != nil {
				t.Fatalf("ScanOpencodeV2Messages: %v", err)
			}
			if got.ToolCalls != tc.wantToolCalls || got.GhostSaves != tc.wantSaves {
				t.Errorf("ScanOpencodeV2Messages = %+v, want toolCalls=%d saves=%d", got, tc.wantToolCalls, tc.wantSaves)
			}
		})
	}

	res, ok, err := Scan(FormatOpencodeV2Messages, strings.NewReader(oc2LineCodeModeSave))
	if !ok || err != nil || res.ToolCalls != 1 || res.GhostSaves != 1 {
		t.Errorf("Scan(opencode-v2-messages) = %+v,%v,%v want 1 tool call, 1 save", res, ok, err)
	}
}

// TestScanOpencodeV2Messages_GoldenFixture pins the scanner against a real
// opencode 2.0.14 session.context transcript: a warm-up `execute`, the Code
// Mode catalog system message (which names ghost_memory_save in prose), a
// Code Mode ghost save, a closing text reply, and the idle marker.
func TestScanOpencodeV2Messages_GoldenFixture(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "opencode-v2-messages", "session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck
	got, err := ScanOpencodeV2Messages(f)
	if err != nil {
		t.Fatalf("ScanOpencodeV2Messages: %v", err)
	}
	if got.ToolCalls != 2 || got.GhostSaves != 1 {
		t.Errorf("golden fixture = %+v, want toolCalls=2 saves=1", got)
	}
}
