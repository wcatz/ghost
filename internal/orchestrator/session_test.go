package orchestrator

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"testing"

	"github.com/wcatz/ghost/internal/ai"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/mode"
	"github.com/wcatz/ghost/internal/project"
	"github.com/wcatz/ghost/internal/prompt"
	"github.com/wcatz/ghost/internal/tool"
)

// testSession creates a minimal Session for testing helper functions.
// It uses a real in-memory SQLite store.
func testSession(t *testing.T) *Session {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store := memory.NewStore(db, logger)

	ctx := context.Background()
	if err := store.EnsureProject(ctx, "test-id", "/tmp/test", "test"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	projCtx := &project.Context{
		ID:   "test-id",
		Path: "/tmp/test",
		Name: "test",
	}

	registry := tool.NewRegistry()
	builder := prompt.NewBuilder(store)

	return NewSession(
		projCtx,
		nil, // LLMProvider (not needed for helper tests)
		store,
		registry,
		builder,
		nil, // reflector
		logger,
		"claude-opus-4-6-20250514",      // quality model
		"claude-sonnet-4-5-20250929",    // fast model
		"chat",
	)
}

func TestNewSession(t *testing.T) {
	s := testSession(t)

	if s.ProjectID != "test-id" {
		t.Errorf("ProjectID = %q, want %q", s.ProjectID, "test-id")
	}
	if s.ProjectPath != "/tmp/test" {
		t.Errorf("ProjectPath = %q, want %q", s.ProjectPath, "/tmp/test")
	}
	if s.ProjectName != "test" {
		t.Errorf("ProjectName = %q, want %q", s.ProjectName, "test")
	}
	if !s.Active {
		t.Error("session should be active")
	}
	if s.Mode.Name != "chat" {
		t.Errorf("Mode.Name = %q, want %q", s.Mode.Name, "chat")
	}
	if s.CreatedAt.IsZero() {
		t.Error("CreatedAt should not be zero")
	}
}

func TestSession_MessageCount(t *testing.T) {
	s := testSession(t)

	if s.MessageCount() != 0 {
		t.Fatalf("initial MessageCount = %d, want 0", s.MessageCount())
	}

	s.mu.Lock()
	s.messages = append(s.messages, ai.TextMessage("user", "hello"))
	s.mu.Unlock()

	if s.MessageCount() != 1 {
		t.Errorf("MessageCount = %d, want 1", s.MessageCount())
	}
}

func TestSession_ClearMessages(t *testing.T) {
	s := testSession(t)

	s.mu.Lock()
	s.messages = append(s.messages, ai.TextMessage("user", "hello"))
	s.messages = append(s.messages, ai.TextMessage("assistant", "hi"))
	s.mu.Unlock()

	if s.MessageCount() != 2 {
		t.Fatalf("MessageCount before clear = %d, want 2", s.MessageCount())
	}

	s.ClearMessages()

	if s.MessageCount() != 0 {
		t.Errorf("MessageCount after clear = %d, want 0", s.MessageCount())
	}
}

func TestSession_SetMode(t *testing.T) {
	s := testSession(t)

	// Default mode is "chat".
	if s.Mode.Name != "chat" {
		t.Fatalf("initial mode = %q, want %q", s.Mode.Name, "chat")
	}

	// Set to unknown mode — should fall back to default.
	s.SetMode("nonexistent")
	if s.Mode.Name != mode.Default() {
		t.Errorf("SetMode(unknown) = %q, want default %q", s.Mode.Name, mode.Default())
	}

	// Set back to chat explicitly.
	s.SetMode("chat")
	if s.Mode.Name != "chat" {
		t.Errorf("SetMode(chat) = %q, want %q", s.Mode.Name, "chat")
	}
}

func TestSession_SetAutoApprove(t *testing.T) {
	s := testSession(t)

	s.SetAutoApprove(true)
	s.mu.Lock()
	auto := s.autoApprove
	s.mu.Unlock()
	if !auto {
		t.Error("expected autoApprove=true after SetAutoApprove(true)")
	}

	s.SetAutoApprove(false)
	s.mu.Lock()
	auto = s.autoApprove
	s.mu.Unlock()
	if auto {
		t.Error("expected autoApprove=false after SetAutoApprove(false)")
	}
}

func TestSession_Model(t *testing.T) {
	s := testSession(t)
	if s.Model() != "claude-sonnet-4-5-20250929" {
		t.Errorf("Model() = %q, want %q", s.Model(), "claude-sonnet-4-5-20250929")
	}
}

func TestSession_Store(t *testing.T) {
	s := testSession(t)
	if s.Store() == nil {
		t.Error("Store() should not be nil")
	}
}

func TestDecodeStoredMessage_PlainText(t *testing.T) {
	m := memory.ConversationMessage{
		Role:    "user",
		Content: "hello world",
	}
	msg := decodeStoredMessage(m)
	if msg.Role != "user" {
		t.Errorf("Role = %q, want %q", msg.Role, "user")
	}
	if len(msg.Content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(msg.Content))
	}
	if msg.Content[0].Type != "text" {
		t.Errorf("block type = %q, want %q", msg.Content[0].Type, "text")
	}
	if msg.Content[0].Text != "hello world" {
		t.Errorf("block text = %q, want %q", msg.Content[0].Text, "hello world")
	}
}

func TestDecodeStoredMessage_JSONBlocks(t *testing.T) {
	blocks := []ai.ContentBlock{
		{Type: "tool_result", ToolUseID: "t1", Content: json.RawMessage(`[{"type":"text","text":"file contents"}]`)},
	}
	raw, _ := json.Marshal(blocks)

	m := memory.ConversationMessage{
		Role:    "user",
		Content: string(raw),
	}
	msg := decodeStoredMessage(m)
	if msg.Role != "user" {
		t.Errorf("Role = %q, want %q", msg.Role, "user")
	}
	if len(msg.Content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(msg.Content))
	}
	if msg.Content[0].Type != "tool_result" {
		t.Errorf("block type = %q, want %q", msg.Content[0].Type, "tool_result")
	}
}

func TestDecodeStoredMessage_InvalidJSON(t *testing.T) {
	m := memory.ConversationMessage{
		Role:    "assistant",
		Content: "not valid json {[",
	}
	msg := decodeStoredMessage(m)
	if msg.Role != "assistant" {
		t.Errorf("Role = %q, want %q", msg.Role, "assistant")
	}
	// Should fall back to plain text.
	if len(msg.Content) != 1 || msg.Content[0].Type != "text" {
		t.Errorf("expected text fallback, got %v", msg.Content)
	}
}

func TestWindowedMessages_ShortConversation(t *testing.T) {
	s := testSession(t)

	s.mu.Lock()
	s.messages = []ai.Message{
		ai.TextMessage("user", "hello"),
		ai.TextMessage("assistant", "hi there"),
	}
	result := s.windowedMessages()
	s.mu.Unlock()

	// Short conversations should pass through (with possible sanitization).
	if len(result) < 2 {
		t.Errorf("expected at least 2 messages, got %d", len(result))
	}
}

func TestWindowedMessages_WithToolUse(t *testing.T) {
	s := testSession(t)

	s.mu.Lock()
	s.messages = []ai.Message{
		ai.TextMessage("user", "read a file"),
		{Role: "assistant", Content: []ai.ContentBlock{
			{Type: "text", Text: "Let me read that"},
			{Type: "tool_use", ID: "t1", Name: "file_read", Input: json.RawMessage(`{"path":"test.go"}`)},
		}},
		ai.ToolResultMessage([]ai.ToolResult{
			{ToolUseID: "t1", Content: "package main"},
		}),
		{Role: "assistant", Content: []ai.ContentBlock{
			{Type: "text", Text: "Here is the file"},
		}},
		ai.TextMessage("user", "thanks"),
	}
	result := s.windowedMessages()
	s.mu.Unlock()

	if len(result) < 5 {
		t.Errorf("expected at least 5 messages, got %d", len(result))
	}
}

func TestEstimateTokens_WithToolInput(t *testing.T) {
	msgs := []ai.Message{
		{Role: "assistant", Content: []ai.ContentBlock{
			{Type: "tool_use", ID: "t1", Name: "grep", Input: json.RawMessage(`{"pattern":"test","path":"/home/user"}`)},
		}},
		ai.ToolResultMessage([]ai.ToolResult{
			{ToolUseID: "t1", Content: "line1\nline2\nline3"},
		}),
	}
	est := estimateTokens(msgs)
	if est <= 0 {
		t.Errorf("estimateTokens with tool input = %d, want > 0", est)
	}
}

func TestAddTurnCaching_NoUserMessages(t *testing.T) {
	msgs := []ai.Message{
		ai.TextMessage("assistant", "a"),
		ai.TextMessage("assistant", "b"),
		ai.TextMessage("assistant", "c"),
		ai.TextMessage("assistant", "d"),
	}
	result := addTurnCaching(msgs)
	// No user messages to cache, should return unmodified.
	for _, m := range result {
		for _, b := range m.Content {
			if b.CacheControl != nil {
				t.Error("should not add cache_control without user messages")
			}
		}
	}
}

func TestPersistMessage_NoConversationID(t *testing.T) {
	s := testSession(t)
	// ConversationID is empty, persistMessage should be a no-op.
	s.persistMessage(context.Background(), "user", "hello")
	// Should not panic or error.
}

// --- sanitizeMessages tests ---

func TestSanitizeMessages_Empty(t *testing.T) {
	result := sanitizeMessages(nil)
	if len(result) != 0 {
		t.Errorf("expected empty, got %d messages", len(result))
	}
}

func TestSanitizeMessages_NoOrphans(t *testing.T) {
	msgs := []ai.Message{
		ai.TextMessage("user", "hello"),
		{Role: "assistant", Content: []ai.ContentBlock{
			{Type: "text", Text: "I'll check that for you"},
			{Type: "tool_use", ID: "tool_1", Name: "file_read"},
		}},
		ai.ToolResultMessage([]ai.ToolResult{
			{ToolUseID: "tool_1", Content: "file contents here"},
		}),
		{Role: "assistant", Content: []ai.ContentBlock{
			{Type: "text", Text: "Here's the file content."},
		}},
	}

	result := sanitizeMessages(msgs)
	if len(result) != 4 {
		t.Fatalf("expected 4 messages (no change), got %d", len(result))
	}
}

func TestSanitizeMessages_OrphanedToolUse(t *testing.T) {
	msgs := []ai.Message{
		ai.TextMessage("user", "read the file"),
		{Role: "assistant", Content: []ai.ContentBlock{
			{Type: "tool_use", ID: "tool_orphan", Name: "file_read"},
		}},
	}

	result := sanitizeMessages(msgs)
	if len(result) != 3 {
		t.Fatalf("expected 3 messages (original 2 + synthetic result), got %d", len(result))
	}

	injected := result[2]
	if injected.Role != "user" {
		t.Errorf("injected message role = %q, want %q", injected.Role, "user")
	}
	if len(injected.Content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(injected.Content))
	}
	if injected.Content[0].Type != "tool_result" {
		t.Errorf("injected block type = %q, want %q", injected.Content[0].Type, "tool_result")
	}
	if injected.Content[0].ToolUseID != "tool_orphan" {
		t.Errorf("injected ToolUseID = %q, want %q", injected.Content[0].ToolUseID, "tool_orphan")
	}
	if !injected.Content[0].IsError {
		t.Error("injected tool_result should have IsError=true")
	}
}

func TestSanitizeMessages_MultipleOrphanedTools(t *testing.T) {
	msgs := []ai.Message{
		ai.TextMessage("user", "do two things"),
		{Role: "assistant", Content: []ai.ContentBlock{
			{Type: "tool_use", ID: "tool_a", Name: "file_read"},
			{Type: "tool_use", ID: "tool_b", Name: "grep"},
		}},
	}

	result := sanitizeMessages(msgs)
	if len(result) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(result))
	}

	injected := result[2]
	if len(injected.Content) != 2 {
		t.Fatalf("expected 2 tool_result blocks, got %d", len(injected.Content))
	}
	ids := map[string]bool{
		injected.Content[0].ToolUseID: true,
		injected.Content[1].ToolUseID: true,
	}
	if !ids["tool_a"] || !ids["tool_b"] {
		t.Errorf("expected tool_a and tool_b, got: %v", ids)
	}
}

func TestSanitizeMessages_TextOnlyAssistant(t *testing.T) {
	msgs := []ai.Message{
		ai.TextMessage("user", "hello"),
		ai.TextMessage("assistant", "hi there"),
	}

	result := sanitizeMessages(msgs)
	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result))
	}
}

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		name string
		msgs []ai.Message
		min  int
		max  int
	}{
		{
			name: "empty",
			msgs: nil,
			min:  0,
			max:  0,
		},
		{
			name: "short text",
			msgs: []ai.Message{ai.TextMessage("user", "hello world")},
			min:  1,
			max:  20,
		},
		{
			name: "longer text",
			msgs: []ai.Message{
				ai.TextMessage("user", "This is a longer message that should produce a higher token count estimate"),
				ai.TextMessage("assistant", "This is the response with some additional text to estimate"),
			},
			min: 20,
			max: 200,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			est := estimateTokens(tc.msgs)
			if est < tc.min || est > tc.max {
				t.Errorf("estimateTokens = %d, want between %d and %d", est, tc.min, tc.max)
			}
		})
	}
}

func TestIsToolResult(t *testing.T) {
	tests := []struct {
		name string
		msg  ai.Message
		want bool
	}{
		{
			name: "user text message",
			msg:  ai.TextMessage("user", "hello"),
			want: false,
		},
		{
			name: "assistant message",
			msg:  ai.TextMessage("assistant", "hi"),
			want: false,
		},
		{
			name: "tool result message",
			msg:  ai.ToolResultMessage([]ai.ToolResult{{ToolUseID: "t1", Content: "ok"}}),
			want: true,
		},
		{
			name: "empty content",
			msg:  ai.Message{Role: "user", Content: nil},
			want: false,
		},
		{
			name: "mixed content",
			msg: ai.Message{Role: "user", Content: []ai.ContentBlock{
				{Type: "tool_result", ToolUseID: "t1", Content: json.RawMessage(`[{"type":"text","text":"ok"}]`)},
				{Type: "text", Text: "extra"},
			}},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isToolResult(tc.msg)
			if got != tc.want {
				t.Errorf("isToolResult = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		maxLen int
		want   string
	}{
		{"short", "hello", 10, "hello"},
		{"exact", "hello", 5, "hello"},
		{"long", "hello world", 5, "hello..."},
		{"empty", "", 5, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := truncate(tc.input, tc.maxLen)
			if got != tc.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tc.input, tc.maxLen, got, tc.want)
			}
		})
	}
}

func TestAddTurnCaching_TooShort(t *testing.T) {
	msgs := []ai.Message{
		ai.TextMessage("user", "hello"),
		ai.TextMessage("assistant", "hi"),
	}
	result := addTurnCaching(msgs)
	for _, m := range result {
		for _, b := range m.Content {
			if b.CacheControl != nil {
				t.Error("expected no cache_control on short conversation")
			}
		}
	}
}

func TestAddTurnCaching_MarksLastUserMessage(t *testing.T) {
	msgs := []ai.Message{
		ai.TextMessage("user", "first question"),
		ai.TextMessage("assistant", "first answer"),
		ai.TextMessage("user", "second question"),
		ai.TextMessage("assistant", "second answer"),
		ai.TextMessage("user", "third question"),
	}

	result := addTurnCaching(msgs)

	found := false
	for i, m := range result {
		if m.Role != "user" {
			continue
		}
		for _, b := range m.Content {
			if b.CacheControl != nil {
				if i == 2 {
					found = true
				} else if i == 4 {
					t.Errorf("last user message (index %d) should not have cache_control", i)
				}
			}
		}
	}
	if !found {
		t.Error("expected cache_control on second-to-last user message")
	}
}

func TestParseFileRefs(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []string
	}{
		{
			name: "no refs",
			text: "just some text",
			want: nil,
		},
		{
			name: "file with extension",
			text: "look at @main.go please",
			want: []string{"main.go"},
		},
		{
			name: "file with path",
			text: "check @internal/memory/store.go",
			want: []string{"internal/memory/store.go"},
		},
		{
			name: "multiple refs",
			text: "@file1.go and @dir/file2.ts",
			want: []string{"file1.go", "dir/file2.ts"},
		},
		{
			name: "@ without file-like name ignored",
			text: "hey @user thanks",
			want: nil,
		},
		{
			name: "bare @",
			text: "@ alone",
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseFileRefs(tc.text)
			if len(got) != len(tc.want) {
				t.Fatalf("parseFileRefs(%q) = %v, want %v", tc.text, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("parseFileRefs(%q)[%d] = %q, want %q", tc.text, i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestSession_InitConversation(t *testing.T) {
	s := testSession(t)
	ctx := context.Background()

	if s.ConversationID != "" {
		t.Fatalf("initial ConversationID should be empty, got %q", s.ConversationID)
	}

	if err := s.InitConversation(ctx); err != nil {
		t.Fatalf("InitConversation: %v", err)
	}

	if s.ConversationID == "" {
		t.Error("ConversationID should be set after InitConversation")
	}
}

func TestSession_Resume_NoConversation(t *testing.T) {
	s := testSession(t)
	ctx := context.Background()

	// Resume with no prior conversations should return an error.
	err := s.Resume(ctx)
	if err == nil {
		t.Error("Resume with no conversations should return error")
	}
}

func TestSession_Resume_WithConversation(t *testing.T) {
	s := testSession(t)
	ctx := context.Background()

	// Create a conversation and add messages.
	if err := s.InitConversation(ctx); err != nil {
		t.Fatalf("InitConversation: %v", err)
	}
	convID := s.ConversationID

	if err := s.store.AppendMessage(ctx, convID, "user", "hello from resume test"); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if err := s.store.AppendMessage(ctx, convID, "assistant", "hi back"); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	// Create a fresh session and resume.
	s2 := testSession(t)
	// Use the same store.
	s2.store = s.store

	if err := s2.Resume(ctx); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	if s2.MessageCount() != 2 {
		t.Errorf("MessageCount after resume = %d, want 2", s2.MessageCount())
	}
}

func TestSession_ExpandFileRefs_NoRefs(t *testing.T) {
	s := testSession(t)
	text := "just a message with no file refs"
	result := s.expandFileRefs(text)
	if result != text {
		t.Errorf("expandFileRefs should return unchanged text, got: %q", result)
	}
}

func TestSession_ExpandFileRefs_NonexistentFile(t *testing.T) {
	s := testSession(t)
	text := "check @nonexistent.go"
	result := s.expandFileRefs(text)
	// File doesn't exist, should return original text without expansion.
	if result != text {
		t.Errorf("expandFileRefs with nonexistent file should return original, got: %q", result)
	}
}

func TestSession_ExpandFileRefs_PathTraversal(t *testing.T) {
	s := testSession(t)
	// Attempt path traversal outside project dir.
	text := "check @../../etc/passwd"
	result := s.expandFileRefs(text)
	// Should not expand a path outside project directory.
	if result != text {
		t.Errorf("expandFileRefs should reject path traversal, got: %q", result)
	}
}

func TestSession_PersistMessage_WithConversation(t *testing.T) {
	s := testSession(t)
	ctx := context.Background()

	if err := s.InitConversation(ctx); err != nil {
		t.Fatalf("InitConversation: %v", err)
	}

	s.persistMessage(ctx, "user", "test persistence")
	s.persistMessage(ctx, "assistant", "acknowledged")

	// Verify messages were persisted.
	msgs, err := s.store.GetConversationMessages(ctx, s.ConversationID)
	if err != nil {
		t.Fatalf("GetConversationMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Errorf("expected 2 persisted messages, got %d", len(msgs))
	}
}
