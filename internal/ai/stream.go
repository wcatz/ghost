package ai

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// parseStream reads SSE events from the Claude API and emits StreamEvents.
// Handles text deltas, tool_use blocks (start, input_json_delta, stop), and usage.
func parseStream(r io.Reader, events chan<- StreamEvent) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	var (
		currentToolID   string
		currentToolName string
		inputAccum      strings.Builder
		usage           TokenUsage
		inThinking      bool
	)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var event streamEventRaw
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}

		switch event.Type {
		case "message_start":
			if event.Message != nil {
				usage.InputTokens = event.Message.Usage.InputTokens
				usage.CacheCreationInputTokens = event.Message.Usage.CacheCreationInputTokens
				usage.CacheReadInputTokens = event.Message.Usage.CacheReadInputTokens
			}

		case "content_block_start":
			if event.ContentBlock == nil {
				continue
			}
			switch event.ContentBlock.Type {
			case "thinking":
				inThinking = true
			case "tool_use":
				currentToolID = event.ContentBlock.ID
				currentToolName = event.ContentBlock.Name
				inputAccum.Reset()
				events <- StreamEvent{
					Type: "tool_use_start",
					ToolUse: &ToolUseEvent{
						ID:   currentToolID,
						Name: currentToolName,
					},
				}
			}

		case "content_block_delta":
			var delta deltaRaw
			if err := json.Unmarshal(event.Delta, &delta); err != nil {
				continue
			}

			switch delta.Type {
			case "thinking_delta":
				events <- StreamEvent{Type: "thinking", Text: delta.Thinking}
			case "text_delta":
				events <- StreamEvent{Type: "text", Text: delta.Text}
			case "input_json_delta":
				inputAccum.WriteString(delta.PartialJSON)
				events <- StreamEvent{
					Type: "tool_input_delta",
					ToolUse: &ToolUseEvent{
						ID:         currentToolID,
						Name:       currentToolName,
						InputDelta: delta.PartialJSON,
					},
				}
			}

		case "content_block_stop":
			if inThinking {
				inThinking = false
			} else if currentToolID != "" {
				fullInput := json.RawMessage(inputAccum.String())
				events <- StreamEvent{
					Type: "tool_use_end",
					ToolUse: &ToolUseEvent{
						ID:        currentToolID,
						Name:      currentToolName,
						InputFull: fullInput,
					},
				}
				currentToolID = ""
				currentToolName = ""
			}

		case "message_delta":
			var delta deltaRaw
			if err := json.Unmarshal(event.Delta, &delta); err != nil {
				continue
			}
			if event.Usage != nil {
				usage.OutputTokens = event.Usage.OutputTokens
			}
			if delta.StopReason != "" {
				events <- StreamEvent{
					Type:       "done",
					StopReason: delta.StopReason,
					Usage:      &usage,
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read stream: %w", err)
	}

	return nil
}
