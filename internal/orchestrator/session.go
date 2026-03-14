package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/wcatz/ghost/internal/ai"
	"github.com/wcatz/ghost/internal/mode"
	"github.com/wcatz/ghost/internal/project"
	"github.com/wcatz/ghost/internal/prompt"
	"github.com/wcatz/ghost/internal/provider"
	"github.com/wcatz/ghost/internal/reflection"
	"github.com/wcatz/ghost/internal/tool"
)

// Session manages a single project's conversation, tools, and memory.
type Session struct {
	ProjectID      string
	ProjectPath    string
	ProjectName    string
	Mode           mode.Mode
	ConversationID string
	Active         bool
	CreatedAt      time.Time
	LastActiveAt   time.Time

	mu         sync.Mutex
	messages   []ai.Message
	projectCtx *project.Context

	client      provider.LLMProvider
	store       provider.MemoryStore
	registry    *tool.Registry
	builder     *prompt.Builder
	reflector   *reflection.Engine
	logger      *slog.Logger
	model       string
	autoApprove bool
}

// NewSession creates a project session.
func NewSession(
	projCtx *project.Context,
	client provider.LLMProvider,
	store provider.MemoryStore,
	registry *tool.Registry,
	builder *prompt.Builder,
	reflector *reflection.Engine,
	logger *slog.Logger,
	model string,
	defaultMode string,
) *Session {
	return &Session{
		ProjectID:    projCtx.ID,
		ProjectPath:  projCtx.Path,
		ProjectName:  projCtx.Name,
		Mode:         mode.Get(defaultMode),
		Active:       true,
		CreatedAt:    time.Now(),
		LastActiveAt: time.Now(),
		projectCtx:   projCtx,
		client:       client,
		store:        store,
		registry:     registry,
		builder:      builder,
		reflector:    reflector,
		logger:       logger,
		model:        model,
	}
}

// ApprovalFunc is the legacy synchronous approval callback.
// Deprecated: Use SendAsync with channel-based approval instead.
type ApprovalFunc func(toolName string, input json.RawMessage) bool

// SendAsync processes a user message through the full agentic loop.
// Approval requests are sent to approvalCh; the frontend must respond via
// the embedded Response channel. The 30s timeout prevents deadlock if the
// frontend dies.
func (s *Session) SendAsync(ctx context.Context, userMsg string, approvalCh chan<- provider.ApprovalRequest) <-chan ai.StreamEvent {
	approvalFn := func(toolName string, input json.RawMessage) bool {
		resp := make(chan bool, 1)
		select {
		case approvalCh <- provider.ApprovalRequest{
			ToolName: toolName,
			Input:    input,
			Response: resp,
		}:
		case <-ctx.Done():
			return false
		}
		select {
		case approved := <-resp:
			return approved
		case <-ctx.Done():
			return false
		case <-time.After(30 * time.Second):
			s.logger.Warn("approval timeout, denying", "tool", toolName)
			return false
		}
	}
	return s.Send(ctx, userMsg, approvalFn)
}

// Send processes a user message through the full agentic loop.
// It streams events through the returned channel.
func (s *Session) Send(ctx context.Context, userMsg string, approvalFn ApprovalFunc) <-chan ai.StreamEvent {
	events := make(chan ai.StreamEvent, 128)

	go func() {
		defer close(events)

		s.mu.Lock()
		s.LastActiveAt = time.Now()

		// Append user message.
		s.messages = append(s.messages, ai.TextMessage("user", userMsg))

		// Build system blocks.
		system := s.builder.BuildSystemBlocks(ctx, s.projectCtx, s.Mode)
		tools := s.registry.Definitions()

		s.mu.Unlock()

		// Agentic loop: send -> tool_use -> execute -> send results -> repeat.
		var fullResponse string
		for {
			s.mu.Lock()
			msgs := make([]ai.Message, len(s.messages))
			copy(msgs, s.messages)
			s.mu.Unlock()

			stream, err := s.client.ChatStream(ctx, msgs, system, tools, s.model, s.Mode.MaxTokens)
			if err != nil {
				events <- ai.StreamEvent{Type: "error", Error: err}
				return
			}

			// Collect response.
			var textAccum string
			var toolCalls []ai.ContentBlock
			var stopReason string
			var usage *ai.TokenUsage

			for evt := range stream {
				switch evt.Type {
				case "text":
					textAccum += evt.Text
					events <- evt
				case "tool_use_start", "tool_input_delta":
					events <- evt
				case "tool_use_end":
					events <- evt
					if evt.ToolUse != nil {
						toolCalls = append(toolCalls, ai.ContentBlock{
							Type:  "tool_use",
							ID:    evt.ToolUse.ID,
							Name:  evt.ToolUse.Name,
							Input: evt.ToolUse.InputFull,
						})
					}
				case "done":
					stopReason = evt.StopReason
					usage = evt.Usage
				case "error":
					events <- evt
					return
				}
			}

			// Build assistant message from accumulated content.
			var assistantBlocks []ai.ContentBlock
			if textAccum != "" {
				assistantBlocks = append(assistantBlocks, ai.ContentBlock{Type: "text", Text: textAccum})
				fullResponse += textAccum
			}
			assistantBlocks = append(assistantBlocks, toolCalls...)

			s.mu.Lock()
			s.messages = append(s.messages, ai.Message{Role: "assistant", Content: assistantBlocks})
			s.mu.Unlock()

			// If no tool calls, we're done.
			if stopReason != "tool_use" || len(toolCalls) == 0 {
				if usage != nil {
					events <- ai.StreamEvent{Type: "done", Usage: usage, StopReason: stopReason}
				}
				break
			}

			// Execute tools and collect results.
			var results []ai.ToolResult
			for _, tc := range toolCalls {
				// Check approval.
				level := s.registry.GetApprovalLevel(tc.Name)
				if level == tool.ApprovalRequire && !s.autoApprove {
					if approvalFn != nil && !approvalFn(tc.Name, tc.Input) {
						results = append(results, ai.ToolResult{
							ToolUseID: tc.ID,
							Content:   "User denied this operation",
							IsError:   true,
						})
						continue
					}
				}

				result := s.registry.Execute(ctx, tc.Name, s.ProjectPath, tc.Input)
				events <- ai.StreamEvent{
					Type: "text",
					Text: fmt.Sprintf("\n<tool_result name=%q duration=%s>\n", tc.Name, result.Duration),
				}

				results = append(results, ai.ToolResult{
					ToolUseID: tc.ID,
					Content:   result.Content,
					IsError:   result.IsError,
				})
			}

			// Append tool results and continue the loop.
			s.mu.Lock()
			s.messages = append(s.messages, ai.ToolResultMessage(results))
			s.mu.Unlock()
		}

		// Post-exchange: memory extraction + reflection (fire and forget).
		if fullResponse != "" {
			go func() {
				defer func() {
					if r := recover(); r != nil {
						s.logger.Error("panic in memory extraction", "error", r)
					}
				}()
				reflection.ExtractMemories(
					context.Background(), s.client, s.store, s.logger,
					s.ProjectID, userMsg, fullResponse,
				)
			}()
			go func() {
				defer func() {
					if r := recover(); r != nil {
						s.logger.Error("panic in reflection", "error", r)
					}
				}()
				s.reflector.MaybeReflect(
					context.Background(), s.ProjectID, s.projectCtx,
				)
			}()
		}
	}()

	return events
}

// SetMode changes the session mode.
func (s *Session) SetMode(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Mode = mode.Get(name)
}

// SetAutoApprove enables or disables auto-approval for all tools.
func (s *Session) SetAutoApprove(auto bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.autoApprove = auto
}

// Refresh re-scans the project context.
func (s *Session) Refresh() error {
	ctx, err := project.Detect(s.ProjectPath)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.projectCtx = ctx
	return nil
}

// ClearMessages resets the conversation (keeps memories).
func (s *Session) ClearMessages() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = nil
}

// MessageCount returns the number of messages in the conversation.
func (s *Session) MessageCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.messages)
}

// Store returns the memory store for direct access (e.g., REPL commands).
func (s *Session) Store() provider.MemoryStore {
	return s.store
}
