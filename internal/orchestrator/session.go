package orchestrator

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wcatz/ghost/internal/ai"
	memstore "github.com/wcatz/ghost/internal/memory"
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
	Cost        ai.CostTracker
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

	// Fix orphaned tool_use blocks from interrupted sessions.
	s.messages = sanitizeMessages(s.messages)

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
func decodeStoredMessage(m memstore.ConversationMessage) ai.Message {
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
// PDF files are sent as document content blocks; all other files are inlined as text.
func (s *Session) Send(ctx context.Context, userMsg string, approvalFn ApprovalFunc) <-chan ai.StreamEvent {
	msg := s.buildUserMessage(userMsg)
	return s.SendMessage(ctx, msg, userMsg, approvalFn)
}

// buildUserMessage creates a user message, expanding @file refs.
// PDFs become document content blocks; text files are appended inline.
func (s *Session) buildUserMessage(text string) ai.Message {
	refs := parseFileRefs(text)
	if len(refs) == 0 {
		return ai.TextMessage("user", text)
	}

	var blocks []ai.ContentBlock
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

		// PDF files → document content block (base64).
		if strings.HasSuffix(strings.ToLower(ref), ".pdf") {
			encoded := base64.StdEncoding.EncodeToString(data)
			blocks = append(blocks, ai.DocumentBlock(encoded))
			fmt.Fprintf(&sb, "\n\n[Attached PDF: %s]", ref)
			continue
		}

		// All other files → inline text.
		content := string(data)
		if len(content) > 50000 {
			content = content[:50000] + "\n... (truncated at 50KB)"
		}
		fmt.Fprintf(&sb, "\n\n---\n**File: %s**\n```\n%s\n```\n", ref, content)
	}

	// Build message: document blocks first, then text.
	var allBlocks []ai.ContentBlock
	allBlocks = append(allBlocks, blocks...)
	allBlocks = append(allBlocks, ai.ContentBlock{Type: "text", Text: sb.String()})
	return ai.Message{Role: "user", Content: allBlocks}
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
					s.Cost.Add(usage)
					// Persist usage to SQLite for all-time cost tracking.
					if usage != nil {
						costUSD := float64(usage.InputTokens)/1e6*ai.SonnetInputPerM +
							float64(usage.OutputTokens)/1e6*ai.SonnetOutputPerM +
							float64(usage.CacheCreationInputTokens)/1e6*ai.SonnetCacheWritePerM +
							float64(usage.CacheReadInputTokens)/1e6*ai.SonnetCacheReadPerM
						_ = s.store.RecordUsage(ctx, s.ProjectID, s.model, memstore.TokenUsage{
							InputTokens:   usage.InputTokens,
							OutputTokens:  usage.OutputTokens,
							CacheCreation: usage.CacheCreationInputTokens,
							CacheRead:     usage.CacheReadInputTokens,
							CostUSD:       costUSD,
						})
					}
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

				// Send tool result for TUI display (before summarization).
				events <- ai.StreamEvent{
					Type: "tool_result",
					ToolUse: &ai.ToolUseEvent{
						ID:   tc.ID,
						Name: tc.Name,
					},
					Text: result.Content,
					Metadata: map[string]string{
						"is_error": fmt.Sprintf("%v", result.IsError),
						"duration": result.Duration.String(),
					},
				}

				// Emit diff metadata for file_edit/file_write tools.
				if result.Metadata != nil {
					events <- ai.StreamEvent{
						Type:     "tool_diff",
						ToolUse:  &ai.ToolUseEvent{ID: tc.ID, Name: tc.Name},
						Metadata: result.Metadata,
					}
				}

				// Summarize large tool results with Haiku to save Sonnet input tokens.
				content := result.Content
				if !result.IsError && len(content) > 10000 {
					if summary, _, err := s.client.Reflect(ctx,
						fmt.Sprintf("Summarize this tool output concisely, preserving all important details (file paths, line numbers, error messages, key values). Do NOT wrap in JSON.\n\nTool: %s\nOutput:\n%s", tc.Name, truncate(content, 30000)),
					); err == nil && len(summary) > 0 {
						content = summary + "\n\n[summarized from " + fmt.Sprintf("%d", len(result.Content)) + " chars by Haiku]"
					}
				}

				results = append(results, ai.ToolResult{
					ToolUseID: tc.ID,
					Content:   content,
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

// compressionThreshold is the token count at which we start compressing.
// Set lower than maxContextTokens to give room for the summary.
const compressionThreshold = 150000

// windowedMessages returns a copy of messages, compressing older exchanges
// into a summary if the estimated token count exceeds compressionThreshold.
// Must be called with s.mu held.
func (s *Session) windowedMessages() []ai.Message {
	msgs := make([]ai.Message, len(s.messages))
	copy(msgs, s.messages)

	msgs = sanitizeMessages(msgs)

	// Multi-turn caching: mark the last user+assistant exchange with
	// cache_control so the API caches everything up to that point.
	// This saves ~90% on input tokens for agentic tool loops.
	msgs = addTurnCaching(msgs)

	est := estimateTokens(msgs)
	if est <= compressionThreshold {
		return msgs
	}

	// Find the split point — keep the most recent messages that fit in half the budget.
	keepTokens := maxContextTokens / 2
	keepStart := len(msgs)
	keepEst := 0
	for keepStart > 0 {
		keepStart--
		keepEst = estimateTokens(msgs[keepStart:])
		if keepEst > keepTokens {
			keepStart++
			break
		}
	}

	// Ensure we don't split in the middle of a tool_use/tool_result pair.
	for keepStart < len(msgs) && isToolResult(msgs[keepStart]) {
		keepStart++
	}

	if keepStart <= 2 {
		// Not enough old messages to compress — just return as-is.
		return msgs
	}

	// Summarize the old messages.
	oldMsgs := msgs[:keepStart]
	summary := s.compressMessages(oldMsgs)

	// Build compressed message list: summary + recent messages.
	compressed := make([]ai.Message, 0, 1+len(msgs)-keepStart)
	compressed = append(compressed, ai.TextMessage("user", "[Conversation summary from earlier in this session]\n\n"+summary))
	compressed = append(compressed, ai.TextMessage("assistant", "Understood. I have the context from our earlier conversation. Let's continue."))
	compressed = append(compressed, msgs[keepStart:]...)

	s.logger.Info("context compressed",
		"original_msgs", len(s.messages),
		"old_msgs_summarized", keepStart,
		"kept_msgs", len(msgs)-keepStart,
		"original_tokens", est,
		"compressed_tokens", estimateTokens(compressed),
	)
	return compressed
}

// compressMessages summarizes a slice of messages into a compact text summary.
func (s *Session) compressMessages(msgs []ai.Message) string {
	var sb strings.Builder
	for _, m := range msgs {
		role := m.Role
		for _, b := range m.Content {
			switch {
			case b.Type == "text" && b.Text != "":
				fmt.Fprintf(&sb, "[%s] %s\n", role, truncate(b.Text, 500))
			case b.Type == "tool_use":
				fmt.Fprintf(&sb, "[%s] tool:%s\n", role, b.Name)
			case b.Type == "tool_result":
				fmt.Fprintf(&sb, "[tool_result] %s\n", truncate(b.Content, 200))
			}
		}
	}

	prompt := fmt.Sprintf("Summarize this conversation concisely. Focus on: what was discussed, what decisions were made, what files were modified, and what the current task is. Be specific about file names and code changes.\n\n%s", sb.String())

	summary, _, err := s.client.Reflect(context.Background(), prompt)
	if err != nil {
		s.logger.Warn("compression reflect failed, using simple summary", "error", err)
		// Fallback: just list the topics.
		return sb.String()
	}
	return summary
}

// truncate shortens text to maxLen, adding "..." if truncated.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
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

// addTurnCaching marks the last user message's content blocks with
// cache_control so the Anthropic API caches all prior conversation turns.
// In an agentic tool loop (send → tool_use → tool_result → send), this
// means subsequent API calls only pay for the new tool results, not the
// entire conversation history. The API allows up to 4 cache breakpoints;
// we use one on conversation turns (system blocks use the other 3).
func addTurnCaching(msgs []ai.Message) []ai.Message {
	if len(msgs) < 4 {
		return msgs // too short to benefit from caching
	}

	// Find the last user message that isn't the very last message
	// (we want to cache everything before the newest exchange).
	cacheIdx := -1
	for i := len(msgs) - 2; i >= 0; i-- {
		if msgs[i].Role == "user" {
			cacheIdx = i
			break
		}
	}
	if cacheIdx < 0 {
		return msgs
	}

	// Clone the message and add cache_control to its last content block.
	m := msgs[cacheIdx]
	blocks := make([]ai.ContentBlock, len(m.Content))
	copy(blocks, m.Content)
	if len(blocks) > 0 {
		last := &blocks[len(blocks)-1]
		last.CacheControl = &ai.CacheControlEphemeral
	}
	msgs[cacheIdx] = ai.Message{Role: m.Role, Content: blocks}
	return msgs
}

// sanitizeMessages fixes orphaned tool_use blocks that have no matching
// tool_result anywhere in the conversation. This happens when a session is
// interrupted mid-tool-execution. Claude's API requires every tool_use block
// to be immediately followed by a user message containing tool_result.
func sanitizeMessages(msgs []ai.Message) []ai.Message {
	if len(msgs) == 0 {
		return msgs
	}

	var sanitized []ai.Message
	for i, m := range msgs {
		sanitized = append(sanitized, m)

		// If this is an assistant message with tool_use blocks,
		// check if the next message has matching tool_results.
		if m.Role != "assistant" {
			continue
		}
		var toolIDs []string
		for _, b := range m.Content {
			if b.Type == "tool_use" {
				toolIDs = append(toolIDs, b.ID)
			}
		}
		if len(toolIDs) == 0 {
			continue
		}

		// Check if the next message is a user message with tool_results.
		hasResults := false
		if i+1 < len(msgs) && msgs[i+1].Role == "user" {
			for _, b := range msgs[i+1].Content {
				if b.Type == "tool_result" {
					hasResults = true
					break
				}
			}
		}

		if !hasResults {
			// Inject synthetic tool_results for all tool_use blocks.
			var results []ai.ToolResult
			for _, id := range toolIDs {
				results = append(results, ai.ToolResult{
					ToolUseID: id,
					Content:   "tool execution was interrupted",
					IsError:   true,
				})
			}
			sanitized = append(sanitized, ai.ToolResultMessage(results))
		}
	}
	return sanitized
}

// Store returns the memory store for direct access (e.g., REPL commands).
func (s *Session) Store() provider.MemoryStore {
	return s.store
}
