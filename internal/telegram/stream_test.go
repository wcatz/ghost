package telegram

import (
	"strings"
	"testing"
	"time"
)

func TestFormatStatusMessage_Basic(t *testing.T) {
	st := &streamState{
		project: "ghost",
		started: time.Now().Add(-5 * time.Second),
	}
	msg := formatStatusMessage(st)
	if !strings.Contains(msg, "ghost") {
		t.Error("should contain project name")
	}
	if !strings.Contains(msg, "⚙") {
		t.Error("should contain working icon")
	}
	if !strings.Contains(msg, "⏱") {
		t.Error("should contain elapsed time")
	}
}

func TestFormatStatusMessage_WithTools(t *testing.T) {
	st := &streamState{
		project: "ghost",
		started: time.Now(),
		tools: []toolLogEntry{
			{Name: "file_read", Status: "done", Duration: "0.3s"},
			{Name: "grep", Status: "running"},
		},
	}
	msg := formatStatusMessage(st)
	// Note: mdv2.Esc escapes underscores, so "file_read" becomes "file\_read".
	if !strings.Contains(msg, "file") {
		t.Error("should contain file_read tool")
	}
	if !strings.Contains(msg, "grep") {
		t.Error("should contain grep tool")
	}
	if !strings.Contains(msg, "✓") {
		t.Error("should contain done checkmark")
	}
	if !strings.Contains(msg, "⏳") {
		t.Error("should contain running indicator")
	}
}

func TestFormatStatusMessage_WithThinking(t *testing.T) {
	st := &streamState{
		project:     "ghost",
		started:     time.Now(),
		thinkingLen: 2048,
	}
	msg := formatStatusMessage(st)
	if !strings.Contains(msg, "💭") {
		t.Error("should contain thinking icon")
	}
	if !strings.Contains(msg, "2048") {
		t.Error("should contain thinking char count")
	}
}

func TestFormatStatusMessage_MaxLength(t *testing.T) {
	st := &streamState{
		project: "ghost",
		started: time.Now(),
	}
	// Add many tools to push over the limit.
	for i := 0; i < 100; i++ {
		st.tools = append(st.tools, toolLogEntry{
			Name:   "very_long_tool_name_that_takes_up_space",
			Status: "done",
		})
	}
	msg := formatStatusMessage(st)
	if len(msg) > maxStatusLen {
		t.Errorf("status message length %d exceeds max %d", len(msg), maxStatusLen)
	}
}

func TestParseStreamData_Text(t *testing.T) {
	evt := parseStreamData("text", `{"text":"hello"}`)
	if evt.Type != "text" || evt.Text != "hello" {
		t.Errorf("got %+v", evt)
	}
}

func TestParseStreamData_Thinking(t *testing.T) {
	evt := parseStreamData("thinking", `{"text":"reasoning..."}`)
	if evt.Type != "thinking" || evt.Text != "reasoning..." {
		t.Errorf("got %+v", evt)
	}
}

func TestParseStreamData_ToolStart(t *testing.T) {
	evt := parseStreamData("tool_use_start", `{"id":"t1","name":"grep"}`)
	if evt.Type != "tool_start" || evt.ToolName != "grep" || evt.ToolID != "t1" {
		t.Errorf("got %+v", evt)
	}
}

func TestParseStreamData_ToolResult(t *testing.T) {
	evt := parseStreamData("tool_result", `{"id":"t1","name":"grep","output":"found","duration":"150ms","is_error":false}`)
	if evt.Type != "tool_result" || evt.ToolName != "grep" || evt.Duration != "150ms" || evt.IsError {
		t.Errorf("got %+v", evt)
	}
}

func TestParseStreamData_ToolResultError(t *testing.T) {
	evt := parseStreamData("tool_result", `{"id":"t1","name":"bash","output":"exit 1","is_error":true}`)
	if !evt.IsError {
		t.Error("expected is_error=true")
	}
}

func TestParseStreamData_ToolDiff(t *testing.T) {
	evt := parseStreamData("tool_diff", `{"id":"t1","name":"file_edit","path":"main.go","diff":"+ line"}`)
	if evt.Type != "tool_diff" || evt.Diff["path"] != "main.go" {
		t.Errorf("got %+v", evt)
	}
}

func TestParseStreamData_Done(t *testing.T) {
	evt := parseStreamData("done", `{"session_cost":"$0.45"}`)
	if evt.Type != "done" || evt.Cost != "$0.45" {
		t.Errorf("got %+v", evt)
	}
}

func TestParseStreamData_Error(t *testing.T) {
	evt := parseStreamData("error", `{"error":"overloaded"}`)
	if evt.Type != "error" || evt.Text != "overloaded" {
		t.Errorf("got %+v", evt)
	}
}

func TestHandleStreamEvt_Accumulation(t *testing.T) {
	tb := &Bot{}
	st := &streamState{started: time.Now()}

	// Text accumulates.
	tb.handleStreamEvt(st, streamEvent{Type: "text", Text: "Hello "})
	tb.handleStreamEvt(st, streamEvent{Type: "text", Text: "world"})
	if got := st.responseText.String(); got != "Hello world" {
		t.Errorf("responseText = %q", got)
	}

	// Thinking accumulates.
	tb.handleStreamEvt(st, streamEvent{Type: "thinking", Text: "hmm..."})
	if st.thinkingLen != 6 {
		t.Errorf("thinkingLen = %d, want 6", st.thinkingLen)
	}

	// Tool start adds entry.
	tb.handleStreamEvt(st, streamEvent{Type: "tool_start", ToolName: "grep"})
	if len(st.tools) != 1 || st.tools[0].Name != "grep" {
		t.Errorf("tools = %+v", st.tools)
	}

	// Tool result updates entry.
	tb.handleStreamEvt(st, streamEvent{Type: "tool_result", ToolName: "grep", Duration: "50ms"})
	if st.tools[0].Status != "done" || st.tools[0].Duration != "50ms" {
		t.Errorf("tool after result = %+v", st.tools[0])
	}

	// Done sets cost.
	tb.handleStreamEvt(st, streamEvent{Type: "done", Cost: "$1.00"})
	if st.cost != "$1.00" {
		t.Errorf("cost = %q", st.cost)
	}

	// Error sets errText.
	tb.handleStreamEvt(st, streamEvent{Type: "error", Text: "boom"})
	if st.errText != "boom" {
		t.Errorf("errText = %q", st.errText)
	}
}

func TestHandleStreamEvt_ToolLogTrimming(t *testing.T) {
	tb := &Bot{}
	st := &streamState{started: time.Now()}

	for i := 0; i < 15; i++ {
		tb.handleStreamEvt(st, streamEvent{Type: "tool_start", ToolName: "tool"})
	}

	if len(st.tools) != maxToolLogEntries {
		t.Errorf("tools length = %d, want %d", len(st.tools), maxToolLogEntries)
	}
}
