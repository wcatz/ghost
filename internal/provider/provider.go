// Package provider defines the core interfaces for Ghost's pluggable components.
package provider

import (
	"context"

	"github.com/wcatz/ghost/internal/ai"
	"github.com/wcatz/ghost/internal/memory"
)

// LLMProvider abstracts LLM interactions for reflection.
type LLMProvider interface {
	Reflect(ctx context.Context, prompt string) (string, ai.TokenUsage, error)
}

// MemoryStore abstracts persistent memory operations.
type MemoryStore interface {
	Create(ctx context.Context, projectID string, m memory.Memory) (string, error)
	Upsert(ctx context.Context, projectID, category, content, source string, importance float32, tags []string) (string, string, float64, error)
	UpsertWithProvenance(ctx context.Context, projectID, category, content, source string, importance float32, tags []string, prov memory.Provenance) (string, string, float64, error)
	UpsertWithOptions(ctx context.Context, projectID, category, content, source string, importance float32, tags []string, opts memory.UpsertOptions) (string, string, float64, error)
	Delete(ctx context.Context, id string) error
	DeleteWithOptions(ctx context.Context, id string, opts memory.DeleteOptions) error
	UpdateMemory(ctx context.Context, projectID, id string, content, category *string, importance *float32, tags []string) error
	PromoteToGlobal(ctx context.Context, projectID, id string) error
	MoveMemory(ctx context.Context, fromProjectID, memoryID, toProjectID string, opts memory.MoveMemoryOptions) (memory.MoveMemoryResult, error)
	ResolveMoveTarget(ctx context.Context, field, to string) (string, error)

	GetTopMemories(ctx context.Context, projectID string, limit int) ([]memory.Memory, error)
	SearchFTS(ctx context.Context, projectID, query string, limit int) ([]memory.Memory, error)
	SearchFTSAll(ctx context.Context, query string, limit int) ([]memory.Memory, error)
	SearchHybrid(ctx context.Context, projectID, query string, queryVec []float32, limit int) ([]memory.Memory, error)
	SearchHybridScoped(ctx context.Context, projectID, query string, queryVec []float32, limit int, scope map[string]string) ([]memory.Memory, error)
	SearchHybridAll(ctx context.Context, query string, queryVec []float32, limit int) ([]memory.Memory, error)
	SearchVector(ctx context.Context, projectID string, queryVec []float32, limit int) ([]memory.ScoredMemory, error)
	GetByCategory(ctx context.Context, projectID, category string, limit int) ([]memory.Memory, error)
	ListMemories(ctx context.Context, projectID, category, retention string, limit int) ([]memory.Memory, error)
	GetByIDs(ctx context.Context, ids []string) ([]memory.Memory, error)
	GetAll(ctx context.Context, projectID string, limit int) ([]memory.Memory, error)
	CountMemories(ctx context.Context, projectID string) (int, error)

	StoreEmbedding(ctx context.Context, memoryID string, vec []float32, model string) error
	DeleteEmbedding(ctx context.Context, memoryID string) error
	UnembeddedMemoryIDs(ctx context.Context, projectID, identity string, limit int) ([]string, error)
	GetMemoryContent(ctx context.Context, id string) (string, error)
	EmbeddingStats(ctx context.Context) (embedded, stale, total int, err error)

	LinkStats(ctx context.Context) (links, scans int, err error)

	PinnedRowsWithContradictions(ctx context.Context, limit int) ([]memory.PinnedContradictedRow, int, error)

	HistoryGrowth(ctx context.Context) (memory.HistoryGrowthResult, error)

	Touch(ctx context.Context, ids []string) error
	TogglePin(ctx context.Context, id string, pinned bool) error
	TogglePinInProject(ctx context.Context, projectID, id string, pinned bool) error

	ReplaceNonManual(ctx context.Context, projectID string, memories []memory.Memory, consolidatedSince string, fed []memory.CorpusRow) ([]string, memory.EmissionDrops, int, map[string]int, error)
	CurrentTimestamp(ctx context.Context) (string, error)

	CreateTask(ctx context.Context, projectID, title, description string, priority int) (string, error)
	GetTask(ctx context.Context, taskID string) (memory.Task, error)
	ListTasks(ctx context.Context, projectID, status string, limit int) ([]memory.Task, error)
	CompleteTask(ctx context.Context, taskID, notes string) (notesClamped bool, err error)
	UpdateTask(ctx context.Context, taskID string, status *string, priority *int, description *string) (memory.Task, error)

	RecordDecision(ctx context.Context, projectID, title, decision, rationale string, alternatives, tags []string) (decisionID, memoryID string, companionClamped bool, err error)
	ListDecisions(ctx context.Context, projectID, status string, limit int) ([]memory.Decision, error)
	LiveDecisions(ctx context.Context, projectID string, limit int) ([]memory.Decision, error)
	FindLiveDecisionByTitle(ctx context.Context, projectID, title string) (string, error)
	SupersedeDecision(ctx context.Context, projectID, oldID, newID string) error
	LiveDecisionSuccessor(ctx context.Context, projectID, decisionID string) (string, error)
	SupersedeDecisionReport(ctx context.Context, projectID, oldID, newID string) (memory.DecisionRetirement, error)
	RecordDecisionSuperseding(ctx context.Context, projectID, title, decision, rationale string, alternatives, tags []string, oldID string) (memory.RecordedDecision, error)

	ListProjects(ctx context.Context) ([]memory.Project, error)
	EnsureProject(ctx context.Context, id, path, name string) error
	EnsureProjectWithRepo(ctx context.Context, id, path, name, repoRemote string) error
	BindNewProjectToCheckout(ctx context.Context, id, dir, name, repoRemote string) (bool, error)
	ResolveOrCreateRepoProject(ctx context.Context, projectRef, repoName, id, path, name, repoRemote string) (string, *memory.BindingRefusal, error)
	ResolveProject(ctx context.Context, input string) (id, name string, err error)
	ResolveExactProjectID(ctx context.Context, id string) (string, bool, error)
	ListProjectNames(ctx context.Context) ([]string, error)
	MergeProject(ctx context.Context, oldID, newID string) error
	DeleteProject(ctx context.Context, input string, apply bool) (memory.DeleteProjectSummary, error)

	IncrementInteraction(ctx context.Context, projectID string) (int, error)
	GetLearnedContext(ctx context.Context, projectID string) (string, error)
	UpdateLearnedContext(ctx context.Context, projectID, learnedContext, summary string) error

	Close() error
}
