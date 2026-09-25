// Package provider defines the core interfaces for Ghost's pluggable components.
package provider

import (
	"context"

	"github.com/wcatz/ghost/internal/ai"
	"github.com/wcatz/ghost/internal/memory"
)

// LLMProvider abstracts LLM interactions for reflection. (A streaming chat
// method lived here until the assistant-era streaming client was removed;
// reflection is the only LLM consumer.)
type LLMProvider interface {
	// Reflect calls a fast model for memory extraction/reflection (via the
	// calling client's CLI harness — claude/opencode/codex/goose subprocess).
	Reflect(ctx context.Context, prompt string) (string, ai.TokenUsage, error)
}

// MemoryStore abstracts persistent memory operations.
type MemoryStore interface {
	// Core CRUD
	Create(ctx context.Context, projectID string, m memory.Memory) (string, error)
	Upsert(ctx context.Context, projectID, category, content, source string, importance float32, tags []string) (string, string, float64, error)
	// UpsertWithProvenance is Upsert plus optional write-time provenance:
	// which harness wrote the memory, in which session, against which
	// reference, and how much it was trusted. A zero Provenance records
	// NULL across all four columns.
	UpsertWithProvenance(ctx context.Context, projectID, category, content, source string, importance float32, tags []string, prov memory.Provenance) (string, string, float64, error)
	// UpsertWithOptions is Upsert plus optional provenance and scope.
	UpsertWithOptions(ctx context.Context, projectID, category, content, source string, importance float32, tags []string, opts memory.UpsertOptions) (string, string, float64, error)
	Delete(ctx context.Context, id string) error
	UpdateMemory(ctx context.Context, projectID, id string, content, category *string, importance *float32, tags []string) error
	PromoteToGlobal(ctx context.Context, projectID, id string) error

	// Queries
	GetTopMemories(ctx context.Context, projectID string, limit int) ([]memory.Memory, error)
	SearchFTS(ctx context.Context, projectID, query string, limit int) ([]memory.Memory, error)
	SearchFTSAll(ctx context.Context, query string, limit int) ([]memory.Memory, error)
	SearchHybrid(ctx context.Context, projectID, query string, queryVec []float32, limit int) ([]memory.Memory, error)
	// SearchHybridScoped is SearchHybrid with a scope constraint applied
	// inside fusion and window selection. The store owns production search
	// parameters so scoped MCP searches retain the configured vector floor.
	SearchHybridScoped(ctx context.Context, projectID, query string, queryVec []float32, limit int, scope map[string]string) ([]memory.Memory, error)
	// ExplainSearch reports an unscoped ranking diagnosis.
	ExplainSearch(ctx context.Context, projectID, query string, queryVec []float32, limit int) (memory.SearchExplain, error)
	// ExplainSearchScoped reports the ranking produced by the same scoped
	// search the formatted path uses, including scope-based exclusions.
	ExplainSearchScoped(ctx context.Context, projectID, query string, queryVec []float32, limit int, scope map[string]string) (memory.SearchExplain, error)
	SearchHybridAll(ctx context.Context, query string, queryVec []float32, limit int) ([]memory.Memory, error)
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
	EmbeddingStats(ctx context.Context) (embedded, total int, err error)

	// Links
	LinkStats(ctx context.Context) (links, scans int, err error)

	// Access tracking
	Touch(ctx context.Context, ids []string) error
	TogglePin(ctx context.Context, id string, pinned bool) error

	// Reflection
	ReplaceNonManual(ctx context.Context, projectID string, memories []memory.Memory, consolidatedSince string) ([]string, error)
	CurrentTimestamp(ctx context.Context) (string, error)

	// Tasks
	CreateTask(ctx context.Context, projectID, title, description string, priority int) (string, error)
	GetTask(ctx context.Context, taskID string) (memory.Task, error)
	ListTasks(ctx context.Context, projectID, status string, limit int) ([]memory.Task, error)
	CompleteTask(ctx context.Context, taskID, notes string) error
	UpdateTask(ctx context.Context, taskID string, status *string, priority *int, description *string) (memory.Task, error)

	// Decisions. companionClamped reports that the composed companion-memory
	// content was cut at memory.MaxContentLen even when no field was.
	RecordDecision(ctx context.Context, projectID, title, decision, rationale string, alternatives, tags []string) (decisionID, memoryID string, companionClamped bool, err error)
	ListDecisions(ctx context.Context, projectID, status string, limit int) ([]memory.Decision, error)
	SupersedeDecision(ctx context.Context, projectID, oldID, newID string) error

	// Project management
	ListProjects(ctx context.Context) ([]memory.Project, error)
	EnsureProject(ctx context.Context, id, path, name string) error
	// EnsureProjectWithRepo is EnsureProject plus the normalized remote of the
	// repository at path, so two checkouts of one repository collapse into a
	// single project. Empty means "no repository known" and never clears a
	// remote already recorded.
	EnsureProjectWithRepo(ctx context.Context, id, path, name, repoRemote string) error
	ResolveProject(ctx context.Context, input string) (id, name string, err error)
	ListProjectNames(ctx context.Context) ([]string, error)
	MergeProject(ctx context.Context, oldID, newID string) error
	DeleteProject(ctx context.Context, input string, apply bool) (memory.DeleteProjectSummary, error)

	// State
	IncrementInteraction(ctx context.Context, projectID string) (int, error)
	GetLearnedContext(ctx context.Context, projectID string) (string, error)
	UpdateLearnedContext(ctx context.Context, projectID, learnedContext, summary string) error

	// Lifecycle
	Close() error
}
