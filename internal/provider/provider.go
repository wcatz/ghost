// Package provider defines the core interfaces for Ghost's pluggable components.
// Each interface can be satisfied by different implementations — the existing code
// becomes the default, and new backends (e.g., Ollama, MCP server) implement the
// same contracts.
package provider

import (
	"context"
	"encoding/json"

	"github.com/wcatz/ghost/internal/ai"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/mode"
	"github.com/wcatz/ghost/internal/project"
)

// LLMProvider abstracts LLM interactions for streaming chat and reflection.
type LLMProvider interface {
	// ChatStream sends a conversation to the LLM and streams events back.
	// thinkingBudget > 0 enables extended thinking with the given token budget.
	ChatStream(
		ctx context.Context,
		messages []ai.Message,
		system []ai.SystemBlock,
		tools []ai.ToolDefinition,
		model string,
		maxTokens int,
		thinkingBudget int,
	) (<-chan ai.StreamEvent, error)

	// Reflect calls a fast model (e.g., Haiku) for memory extraction/reflection.
	// Returns the response text and token usage.
	Reflect(ctx context.Context, prompt string) (string, ai.TokenUsage, error)
}

// MemoryStore abstracts persistent memory operations.
type MemoryStore interface {
	// Core CRUD
	Create(ctx context.Context, projectID string, m memory.Memory) (string, error)
	Upsert(ctx context.Context, projectID, category, content, source string, importance float32, tags []string) (string, bool, error)
	Delete(ctx context.Context, id string) error

	// Queries
	GetTopMemories(ctx context.Context, projectID string, limit int) ([]memory.Memory, error)
	SearchFTS(ctx context.Context, projectID, query string, limit int) ([]memory.Memory, error)
	SearchHybrid(ctx context.Context, projectID, query string, queryVec []float32, limit int) ([]memory.Memory, error)
	SearchVector(ctx context.Context, projectID string, queryVec []float32, limit int) ([]memory.ScoredMemory, error)
	GetByCategory(ctx context.Context, projectID, category string, limit int) ([]memory.Memory, error)
	GetByIDs(ctx context.Context, ids []string) ([]memory.Memory, error)
	GetAll(ctx context.Context, projectID string, limit int) ([]memory.Memory, error)
	CountMemories(ctx context.Context, projectID string) (int, error)

	// Embeddings
	StoreEmbedding(ctx context.Context, memoryID string, vec []float32, model string) error
	DeleteEmbedding(ctx context.Context, memoryID string) error
	UnembeddedMemoryIDs(ctx context.Context, projectID string, limit int) ([]string, error)
	GetMemoryContent(ctx context.Context, id string) (string, error)

	// Access tracking
	Touch(ctx context.Context, ids []string) error
	TogglePin(ctx context.Context, id string, pinned bool) error

	// Reflection
	ReplaceNonManual(ctx context.Context, projectID string, memories []memory.Memory) error

	// Project management
	ListProjects(ctx context.Context) ([]memory.Project, error)
	EnsureProject(ctx context.Context, id, path, name string) error

	// Conversation persistence
	CreateConversation(ctx context.Context, projectID, mode string) (string, error)
	AppendMessage(ctx context.Context, conversationID, role, content string) error
	GetRecentExchanges(ctx context.Context, projectID string, limit int) ([][2]string, error)
	GetLatestConversation(ctx context.Context, projectID string) (string, error)
	GetConversationMessages(ctx context.Context, conversationID string) ([]memory.ConversationMessage, error)

	// State
	IncrementInteraction(ctx context.Context, projectID string) (int, error)
	GetLearnedContext(ctx context.Context, projectID string) (string, error)
	UpdateLearnedContext(ctx context.Context, projectID, learnedContext, summary string) error

	// Cost tracking
	RecordUsage(ctx context.Context, projectID, model string, usage memory.TokenUsage) error

	// Lifecycle
	Close() error
}

// PromptBuilder constructs the system prompt blocks for a given context.
type PromptBuilder interface {
	BuildSystemBlocks(ctx context.Context, projCtx *project.Context, m mode.Mode) []ai.SystemBlock
}

// ApprovalResponse carries the user's decision and optional instructions.
type ApprovalResponse struct {
	Approved     bool
	Instructions string // non-empty when denying with feedback
}

// ApprovalRequest is sent from the agentic loop to the frontend when a tool
// needs user approval. The frontend writes an ApprovalResponse to the channel.
type ApprovalRequest struct {
	ToolName string
	Input    json.RawMessage
	Response chan<- ApprovalResponse
}

// ApprovalFunc is the legacy synchronous approval callback.
// Deprecated: Use ApprovalRequest channel-based flow instead.
type ApprovalFunc func(toolName string, input json.RawMessage) ApprovalResponse

// InputSource produces user text (keyboard, voice transcription, etc.).
type InputSource interface {
	Text() <-chan string
	State() InputState
}

// InputState represents the current state of an input source.
type InputState int

const (
	InputIdle        InputState = iota // Not active
	InputListening                     // Capturing input (e.g., microphone recording)
	InputTranscribing                  // Processing input (e.g., STT)
)

// OutputSink consumes assistant text (display, TTS, etc.).
type OutputSink interface {
	Receive(text string)
	Flush()
}

// Frontend renders agent output and handles user input.
type Frontend interface {
	// Run starts the frontend event loop. Blocks until done.
	Run(ctx context.Context) error
}
