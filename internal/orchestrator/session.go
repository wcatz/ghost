package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wcatz/ghost/internal/ai"
	"github.com/wcatz/ghost/internal/memory"
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

// InitConversation creates a conversation record in SQLite for message persistence.
func (s *Session) InitConversation(ctx context.Context) error {
	convID, err := s.store.CreateConversation(ctx, s.ProjectID, s.Mode.Name)
	if err != nil {
		return fmt.Errorf("create conversation: %w", err)
	}
	s.ConversationID = convID
	return nil
}

// Resume loads the latest conversation from SQLite and restores messages.
func (s *Session) Resume(ctx context.Context) error {
	convID, err := s.store.GetLatestConversation(ctx, s.ProjectID)
	if err != nil {
		return fmt.Errorf("get latest conversation: %w", err)
	}
	s.ConversationID = convID

	stored, err := s.store.GetConversationMessages(ctx, convID)
	if err != nil {
		return fmt.Errorf("load messages: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, m := range stored {
		msg := decodeStoredMessage(m)
		if msg.Role != "" {
			s.messages = append(s.messages, msg)
		}
	}
	s.logger.Info("session resumed", "project", s.ProjectName, "messages", len(s.messages), "conversation", convID[:8])
	return nil
}

// persistMessage saves a message to SQLite if we have a conversation ID.
func (s *Session) persistMessage(ctx context.Context, role, content string) {
	if s.ConversationID == "" {
		return
	}
	if err := s.store.AppendMessage(ctx, s.ConversationID, role, content); err != nil {
		s.logger.Error("persist message", "error", err)
	}
}

// decodeStoredMessage converts a stored message back to an ai.Message.
func decodeStoredMessage(m memory.ConversationMessage) ai.Message {
	// Try JSON decode first (tool_result, multi-block messages).
	var blocks []ai.ContentBlock
	if err := json.Unmarshal([]byte(m.Content), &blocks); err == nil && len(blocks) > 0 {
		return ai.Message{Role: m.Role, Content: blocks}
	}
	// Plain text message.
	return ai.TextMessage(m.Role, m.Content)
}

// ApprovalFunc is the legacy synchronous approval callback.
// Deprecated: Use SendAsync with channel-based approval instead.
type ApprovalFunc func(toolName string, input json.RawMessage) provider.ApprovalResponse

// SendAsync processes a user message through the full agentic loop.
// Approval requests are sent to approvalCh; the frontend must respond via
// the embedded Response channel. The 30s timeout prevents deadlock if the
// frontend dies.
func (s *Session) SendAsync(ctx context.Context, userMsg string, approvalCh chan<- provider.ApprovalRequest) <-chan ai.StreamEvent {
	approvalFn := func(toolName string, input json.RawMessage) provider.ApprovalResponse {
		resp := make(chan provider.ApprovalResponse, 1)
		select {
		case approvalCh <- provider.ApprovalRequest{
			ToolName: toolName,
			Input:    input,
			Response: resp,
		}:
		case <-ctx.Done():
			return provider.ApprovalResponse{Approved: false}
		}
		select {
		case ar := <-resp:
			return ar
		case <-ctx.Done():
			return provider.ApprovalResponse{Approved: false}
		case <-time.After(30 * time.Second):
			s.logger.Warn("approval timeout, denying", "tool", toolName)
			return provider.ApprovalResponse{Approved: false}
		}
	}
	return s.Send(ctx, userMsg, approvalFn)
}

// SendImageAsync sends a user message with an attached image through the agentic loop.
func (s *Session) SendImageAsync(ctx context.Context, text, mediaType, base64Data string, approvalCh chan<- provider.ApprovalRequest) <-chan ai.StreamEvent {
	approvalFn := func(toolName string, input json.RawMessage) provider.ApprovalResponse {
		resp := make(chan provider.ApprovalResponse, 1)
		select {
		case approvalCh <- provider.ApprovalRequest{
			ToolName: toolName,
			Input:    input,
			Response: resp,
		}:
		case <-ctx.Done():
			return provider.ApprovalResponse{Approved: false}
		}
		select {
		case ar := <-resp:
			return ar
		case <-ctx.Done():
			return provider.ApprovalResponse{Approved: false}
		case <-time.After(30 * time.Second):
			s.logger.Warn("approval timeout, denying", "tool", toolName)
			return provider.ApprovalResponse{Approved: false}
		}
	}
	imageBlocks := []ai.ContentBlock{ai.ImageBlock(mediaType, base64Data)}
	msg := ai.MultimodalMessage(text, imageBlocks)
	return s.SendMessage(ctx, msg, text, approvalFn)
}

// Send processes a user message through the full agentic loop.
// It streams events through the returned channel.
// Expands @file references to inline file contents before sending.
func (s *Session) Send(ctx context.Context, userMsg string, approvalFn ApprovalFunc) <-chan ai.StreamEvent {
	expanded := s.expandFileRefs(userMsg)
	return s.SendMessage(ctx, ai.TextMessage("user", expanded), userMsg, approvalFn)
}

// expandFileRefs finds @path references in user text and appends file contents.
// Paths are resolved relative to the project directory.
func (s *Session) expandFileRefs(text string) string {
	refs := parseFileRefs(text)
	if len(refs) == 0 {
		return text
	}

	var sb strings.Builder
	sb.WriteString(text)

	for _, ref := range refs {
		absPath := ref
		if !filepath.IsAbs(ref) {
			absPath = filepath.Join(s.ProjectPath, ref)
		}

		// Validate path is within project directory.
		cleaned := filepath.Clean(absPath)
		if !strings.HasPrefix(cleaned, filepath.Clean(s.ProjectPath)) {
			continue
		}

		data, err := os.ReadFile(cleaned)
		if err != nil {
			s.logger.Debug("@file not found", "path", ref, "error", err)
			continue
		}

		// Cap at 50KB per file to avoid blowing up context.
		content := string(data)
		if len(content) > 50000 {
			content = content[:50000] + "\n... (truncated at 50KB)"
		}

		fmt.Fprintf(&sb, "\n\n---\n**File: %s**\n```\n%s\n```\n", ref, content)
	}
	return sb.String()
}

// parseFileRefs extracts @path references from text.
// Matches @path/to/file.ext patterns (alphanumeric, slashes, dots, dashes, underscores).
func parseFileRefs(text string) []string {
	var refs []string
	words := strings.Fields(text)
	for _, w := range words {
		if strings.HasPrefix(w, "@") && len(w) > 1 {
			ref := strings.TrimPrefix(w, "@")
			// Must look like a file path (has an extension or slash).
			if strings.Contains(ref, ".") || strings.Contains(ref, "/") {
				refs = append(refs, ref)
			}
		}
	}
	return refs
}

// SendMessage processes a pre-built user message through the agentic loop.
// userText is the plain text for memory extraction.
func (s *Session) SendMessage(ctx context.Context, userMessage ai.Message, userText string, approvalFn ApprovalFunc) <-chan ai.StreamEvent {
	events := make(chan ai.StreamEvent, 128)

	go func() {
		defer close(events)

		s.mu.Lock()
		s.LastActiveAt = time.Now()

		// Ensure conversation exists for persistence.
		if s.ConversationID == "" {
			if err := s.InitConversation(ctx); err != nil {
				s.logger.Error("init conversation", "error", err)
			}
		}

		// Append user message.
		s.messages = append(s.messages, userMessage)
		s.persistMessage(ctx, "user", userText)

		// Build system blocks.
		system := s.builder.BuildSystemBlocks(ctx, s.projectCtx, s.Mode)
		tools := s.registry.Definitions()

		s.mu.Unlock()

		// Agentic loop: send -> tool_use -> execute -> send results -> repeat.
		var fullResponse string
		for {
			s.mu.Lock()
			msgs := s.windowedMessages()
			s.mu.Unlock()

			stream, err := s.client.ChatStream(ctx, msgs, system, tools, s.model, s.Mode.MaxTokens, s.Mode.ThinkingBudget)
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
				case "thinking":
					events <- evt // pass thinking to TUI but don't accumulate
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

			// Persist assistant response.
			if textAccum != "" {
				s.persistMessage(ctx, "assistant", textAccum)
			}

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
					if approvalFn != nil {
						ar := approvalFn(tc.Name, tc.Input)
						if !ar.Approved {
							msg := "User denied this operation"
							if ar.Instructions != "" {
								msg = "User denied with instructions: " + ar.Instructions
							}
							results = append(results, ai.ToolResult{
								ToolUseID: tc.ID,
								Content:   msg,
								IsError:   true,
							})
							continue
						}
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
					s.ProjectID, userText, fullResponse,
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

// maxContextTokens is the target context budget. Messages are trimmed to stay
// under this when the conversation grows long. Memories and system prompt are
// separate, so this covers only the messages array.
const maxContextTokens = 180000

// windowedMessages returns a copy of messages, trimming the oldest exchanges
// if the estimated token count exceeds maxContextTokens. Must be called with
// s.mu held.
func (s *Session) windowedMessages() []ai.Message {
	msgs := make([]ai.Message, len(s.messages))
	copy(msgs, s.messages)

	est := estimateTokens(msgs)
	if est <= maxContextTokens {
		return msgs
	}

	// Trim oldest user+assistant pairs (skip tool_result messages that follow).
	// Keep at least the last 4 messages to preserve recent context.
	for est > maxContextTokens && len(msgs) > 4 {
		msgs = msgs[2:] // drop one user+assistant pair
		// Skip any orphaned tool_result messages at the start.
		for len(msgs) > 0 && isToolResult(msgs[0]) {
			msgs = msgs[1:]
		}
		est = estimateTokens(msgs)
	}

	if len(msgs) < len(s.messages) {
		s.logger.Info("context windowed",
			"original", len(s.messages), "trimmed", len(msgs),
			"est_tokens", est)
	}
	return msgs
}

// estimateTokens gives a rough token count. Claude averages ~4 chars per token
// for English text. This is intentionally conservative (slightly overestimates).
func estimateTokens(msgs []ai.Message) int {
	total := 0
	for _, m := range msgs {
		for _, b := range m.Content {
			total += len(b.Text)/3 + len(b.Content)/3
			if b.Input != nil {
				total += len(b.Input) / 3
			}
		}
	}
	return total
}

// isToolResult checks if a message contains only tool_result blocks.
func isToolResult(m ai.Message) bool {
	if m.Role != "user" || len(m.Content) == 0 {
		return false
	}
	for _, b := range m.Content {
		if b.Type != "tool_result" {
			return false
		}
	}
	return true
}

// Store returns the memory store for direct access (e.g., REPL commands).
func (s *Session) Store() provider.MemoryStore {
	return s.store
}
