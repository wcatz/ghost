package ai

import (
	"strings"
	"testing"
)

func TestParseStream_TextDeltas(t *testing.T) {
	input := `data: {"type":"message_start","message":{"usage":{"input_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}

data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}

data: {"type":"content_block_stop","index":0}

data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

data: [DONE]

`

	events := make(chan StreamEvent, 10)
	r := strings.NewReader(input)

	go func() {
		defer close(events)
		if err := parseStream(r, events); err != nil {
			t.Errorf("parseStream error: %v", err)
		}
	}()

	var collected []StreamEvent
	for event := range events {
		collected = append(collected, event)
	}

	// Expect: 2 text events + 1 done event
	if len(collected) < 3 {
		t.Fatalf("expected at least 3 events, got %d", len(collected))
	}

	// Check text events
	textCount := 0
	for _, e := range collected {
		if e.Type == "text" {
			textCount++
		}
	}
	if textCount != 2 {
		t.Errorf("expected 2 text events, got %d", textCount)
	}

	// Check done event
	var doneEvent *StreamEvent
	for i := range collected {
		if collected[i].Type == "done" {
			doneEvent = &collected[i]
			break
		}
	}
	if doneEvent == nil {
		t.Fatal("expected done event")
	}
	if doneEvent.StopReason != "end_turn" {
		t.Errorf("expected stop_reason 'end_turn', got %q", doneEvent.StopReason)
	}
	if doneEvent.Usage == nil {
		t.Fatal("expected usage in done event")
	}
	if doneEvent.Usage.InputTokens != 10 {
		t.Errorf("expected 10 input tokens, got %d", doneEvent.Usage.InputTokens)
	}
	if doneEvent.Usage.OutputTokens != 2 {
		t.Errorf("expected 2 output tokens, got %d", doneEvent.Usage.OutputTokens)
	}
}

func TestParseStream_ThinkingDeltas(t *testing.T) {
	input := `data: {"type":"message_start","message":{"usage":{"input_tokens":20,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}

data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me think..."}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":" about this problem."}}

data: {"type":"content_block_stop","index":0}

data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":10}}

data: [DONE]

`

	events := make(chan StreamEvent, 10)
	r := strings.NewReader(input)

	go func() {
		defer close(events)
		if err := parseStream(r, events); err != nil {
			t.Errorf("parseStream error: %v", err)
		}
	}()

	var collected []StreamEvent
	for event := range events {
		collected = append(collected, event)
	}

	thinkingCount := 0
	for _, e := range collected {
		if e.Type == "thinking" {
			thinkingCount++
		}
	}

	if thinkingCount != 2 {
		t.Errorf("expected 2 thinking events, got %d", thinkingCount)
	}
}

func TestParseStream_ToolUse(t *testing.T) {
	input := `data: {"type":"message_start","message":{"usage":{"input_tokens":100,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}

data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_123","name":"file_read"}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"test.txt\"}"}}

data: {"type":"content_block_stop","index":0}

data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":20}}

data: [DONE]

`

	events := make(chan StreamEvent, 10)
	r := strings.NewReader(input)

	go func() {
		defer close(events)
		if err := parseStream(r, events); err != nil {
			t.Errorf("parseStream error: %v", err)
		}
	}()

	var collected []StreamEvent
	for event := range events {
		collected = append(collected, event)
	}

	// Find tool_use_start
	var startEvent *StreamEvent
	for i := range collected {
		if collected[i].Type == "tool_use_start" {
			startEvent = &collected[i]
			break
		}
	}
	if startEvent == nil {
		t.Fatal("expected tool_use_start event")
	}
	if startEvent.ToolUse == nil {
		t.Fatal("expected ToolUse in start event")
	}
	if startEvent.ToolUse.ID != "toolu_123" {
		t.Errorf("expected ID 'toolu_123', got %q", startEvent.ToolUse.ID)
	}
	if startEvent.ToolUse.Name != "file_read" {
		t.Errorf("expected Name 'file_read', got %q", startEvent.ToolUse.Name)
	}

	// Find tool_input_delta events
	deltaCount := 0
	for _, e := range collected {
		if e.Type == "tool_input_delta" {
			deltaCount++
		}
	}
	if deltaCount != 2 {
		t.Errorf("expected 2 tool_input_delta events, got %d", deltaCount)
	}

	// Find tool_use_end
	var endEvent *StreamEvent
	for i := range collected {
		if collected[i].Type == "tool_use_end" {
			endEvent = &collected[i]
			break
		}
	}
	if endEvent == nil {
		t.Fatal("expected tool_use_end event")
	}
	if endEvent.ToolUse == nil {
		t.Fatal("expected ToolUse in end event")
	}
	if len(endEvent.ToolUse.InputFull) == 0 {
		t.Error("expected non-empty InputFull")
	}

	// Check done event has tool_use stop reason
	var doneEvent *StreamEvent
	for i := range collected {
		if collected[i].Type == "done" {
			doneEvent = &collected[i]
			break
		}
	}
	if doneEvent == nil {
		t.Fatal("expected done event")
	}
	if doneEvent.StopReason != "tool_use" {
		t.Errorf("expected stop_reason 'tool_use', got %q", doneEvent.StopReason)
	}
}

func TestParseStream_CacheHit(t *testing.T) {
	input := `data: {"type":"message_start","message":{"usage":{"input_tokens":50,"cache_creation_input_tokens":0,"cache_read_input_tokens":1000}}}

data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"cached response"}}

data: {"type":"content_block_stop","index":0}

data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}

data: [DONE]

`

	events := make(chan StreamEvent, 10)
	r := strings.NewReader(input)

	go func() {
		defer close(events)
		if err := parseStream(r, events); err != nil {
			t.Errorf("parseStream error: %v", err)
		}
	}()

	var collected []StreamEvent
	for event := range events {
		collected = append(collected, event)
	}

	var doneEvent *StreamEvent
	for i := range collected {
		if collected[i].Type == "done" {
			doneEvent = &collected[i]
			break
		}
	}

	if doneEvent == nil {
		t.Fatal("expected done event")
	}
	if doneEvent.Usage == nil {
		t.Fatal("expected usage")
	}
	if doneEvent.Usage.CacheReadInputTokens != 1000 {
		t.Errorf("expected 1000 cache read tokens, got %d", doneEvent.Usage.CacheReadInputTokens)
	}
}

func TestParseStream_CacheMiss(t *testing.T) {
	input := `data: {"type":"message_start","message":{"usage":{"input_tokens":50,"cache_creation_input_tokens":1500,"cache_read_input_tokens":0}}}

data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}

data: [DONE]

`

	events := make(chan StreamEvent, 10)
	r := strings.NewReader(input)

	go func() {
		defer close(events)
		if err := parseStream(r, events); err != nil {
			t.Errorf("parseStream error: %v", err)
		}
	}()

	var collected []StreamEvent
	for event := range events {
		collected = append(collected, event)
	}

	var doneEvent *StreamEvent
	for i := range collected {
		if collected[i].Type == "done" {
			doneEvent = &collected[i]
			break
		}
	}

	if doneEvent == nil {
		t.Fatal("expected done event")
	}
	if doneEvent.Usage == nil {
		t.Fatal("expected usage")
	}
	if doneEvent.Usage.CacheCreationInputTokens != 1500 {
		t.Errorf("expected 1500 cache creation tokens, got %d", doneEvent.Usage.CacheCreationInputTokens)
	}
}

func TestParseStream_InvalidJSON(t *testing.T) {
	input := `data: {"type":"message_start","message":{"usage":{"input_tokens":10}}}

data: invalid json here

data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}

data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}

data: [DONE]

`

	events := make(chan StreamEvent, 10)
	r := strings.NewReader(input)

	go func() {
		defer close(events)
		// Should not return error, just skip invalid lines
		if err := parseStream(r, events); err != nil {
			t.Errorf("parseStream error: %v", err)
		}
	}()

	var collected []StreamEvent
	for event := range events {
		collected = append(collected, event)
	}

	// Should still get valid events
	textCount := 0
	for _, e := range collected {
		if e.Type == "text" {
			textCount++
		}
	}
	if textCount != 1 {
		t.Errorf("expected 1 text event despite invalid JSON, got %d", textCount)
	}
}

func TestParseStream_EmptyStream(t *testing.T) {
	input := `data: [DONE]

`

	events := make(chan StreamEvent, 10)
	r := strings.NewReader(input)

	go func() {
		defer close(events)
		if err := parseStream(r, events); err != nil {
			t.Errorf("parseStream error: %v", err)
		}
	}()

	var collected []StreamEvent
	for event := range events {
		collected = append(collected, event)
	}

	if len(collected) != 0 {
		t.Errorf("expected 0 events for empty stream, got %d", len(collected))
	}
}

func TestParseStream_MultipleToolCalls(t *testing.T) {
	input := `data: {"type":"message_start","message":{"usage":{"input_tokens":200,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}

data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tool_1","name":"file_read"}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"a.txt\"}"}}

data: {"type":"content_block_stop","index":0}

data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tool_2","name":"grep"}}

data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"pattern\":\"test\"}"}}

data: {"type":"content_block_stop","index":1}

data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":40}}

data: [DONE]

`

	events := make(chan StreamEvent, 20)
	r := strings.NewReader(input)

	go func() {
		defer close(events)
		if err := parseStream(r, events); err != nil {
			t.Errorf("parseStream error: %v", err)
		}
	}()

	var collected []StreamEvent
	for event := range events {
		collected = append(collected, event)
	}

	// Count tool_use_start events
	startCount := 0
	endCount := 0
	for _, e := range collected {
		if e.Type == "tool_use_start" {
			startCount++
		}
		if e.Type == "tool_use_end" {
			endCount++
		}
	}

	if startCount != 2 {
		t.Errorf("expected 2 tool_use_start events, got %d", startCount)
	}
	if endCount != 2 {
		t.Errorf("expected 2 tool_use_end events, got %d", endCount)
	}
}
