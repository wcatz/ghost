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
	// Delete keeps the memory's history; DeleteWithOptions is the redaction path,
	// which removes the history rows in the same transaction.
	Delete(ctx context.Context, id string) error
	DeleteWithOptions(ctx context.Context, id string, opts memory.DeleteOptions) error
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
	SearchHybridAll(ctx context.Context, query string, queryVec []float32, limit int) ([]memory.Memory, error)
	SearchVector(ctx context.Context, projectID string, queryVec []float32, limit int) ([]memory.ScoredMemory, error)
	GetByCategory(ctx context.Context, projectID, category string, limit int) ([]memory.Memory, error)
	// ListMemories is the browse read: a project's memories narrowed by category
	// and/or retention tier, with an empty filter meaning no filter on that axis.
	// It exists beside GetByCategory because the two filters are independent and
	// every combination is a legitimate question an operator asks; a filter
	// applied to an already-trimmed result would report the rows it dropped as
	// absent.
	ListMemories(ctx context.Context, projectID, category, retention string, limit int) ([]memory.Memory, error)
	GetByIDs(ctx context.Context, ids []string) ([]memory.Memory, error)
	GetAll(ctx context.Context, projectID string, limit int) ([]memory.Memory, error)
	CountMemories(ctx context.Context, projectID string) (int, error)

	// Embeddings
	StoreEmbedding(ctx context.Context, memoryID string, vec []float32, model string) error
	DeleteEmbedding(ctx context.Context, memoryID string) error
	// UnembeddedMemoryIDs returns the rows that still need a vector for the
	// given identity, which includes rows whose vector was recorded under a
	// different one. An empty identity means any recorded vector counts.
	UnembeddedMemoryIDs(ctx context.Context, projectID, identity string, limit int) ([]string, error)
	GetMemoryContent(ctx context.Context, id string) (string, error)
	// EmbeddingStats reports coverage as (embedded, stale, total): embedded
	// counts vectors this process can search with, stale counts vectors
	// recorded under a retired identity (awaiting a re-embed), total counts
	// memories. stale is never counted as embedded.
	EmbeddingStats(ctx context.Context) (embedded, stale, total int, err error)

	// Links
	LinkStats(ctx context.Context) (links, scans int, err error)

	// PinnedRowsWithContradictions returns up to limit pinned memories a newer
	// memory contradicts through a live edge, and the total there are.
	PinnedRowsWithContradictions(ctx context.Context, limit int) ([]memory.PinnedContradictedRow, int, error)

	// HistoryGrowth reports how fast memory_history is growing into its
	// retention caps and how much of that growth is version rows that restated
	// the version before them. Read-only, store-wide (the caps are), and the
	// same read `ghost mcp status` prints, so the two cannot disagree.
	HistoryGrowth(ctx context.Context) (memory.HistoryGrowthResult, error)

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
	// FindLiveDecisionByTitle returns the id of a non-superseded decision in
	// the project with the same title (trimmed, case-insensitive), or "".
	FindLiveDecisionByTitle(ctx context.Context, projectID, title string) (string, error)
	SupersedeDecision(ctx context.Context, projectID, oldID, newID string) error
	// SupersedeDecisionReport is SupersedeDecision plus what happened to the old
	// decision's companion memory (retired, or left live because it is pinned).
	SupersedeDecisionReport(ctx context.Context, projectID, oldID, newID string) (memory.DecisionRetirement, error)

	// Project management
	ListProjects(ctx context.Context) ([]memory.Project, error)
	EnsureProject(ctx context.Context, id, path, name string) error
	// EnsureProjectWithRepo is EnsureProject plus the normalized remote of the
	// repository at path, so two checkouts of one repository collapse into a
	// single project. Empty means "no repository known" and never clears a
	// remote already recorded.
	EnsureProjectWithRepo(ctx context.Context, id, path, name, repoRemote string) error
	// BindNewProjectToCheckout opens a NEW project at a checkout in one write
	// transaction and reports whether it did; a claimed id, path or remote
	// answers false with no write and never merges.
	BindNewProjectToCheckout(ctx context.Context, id, dir, name, repoRemote string) (bool, error)
	// ResolveOrCreateRepoProject resolves repository identity and creates the
	// fallback project in one write transaction. projectRef is the ordinary
	// resolved id/path; repoName is derived from the remote, not a directory.
	// path must be the session's directory on this filesystem: a project of the
	// same name is claimed by its unique name only when its recorded path is
	// unusable or contains path, so a synthetic path disables that fallback.
	// A non-nil *memory.BindingRefusal beside the returned id means the
	// unique-name fallback was not allowed to bind the repository to the project
	// its name matched. The save is still routed to the returned id; the refusal
	// names the project that kept the name, and only the caller can report that.
	ResolveOrCreateRepoProject(ctx context.Context, projectRef, repoName, id, path, name, repoRemote string) (string, *memory.BindingRefusal, error)
	ResolveProject(ctx context.Context, input string) (id, name string, err error)
	// ResolveExactProjectID reports whether input is literally a project's id,
	// with no path, remote or basename fallback. The write side needs it to
	// avoid re-deriving an id the caller already supplied.
	ResolveExactProjectID(ctx context.Context, id string) (string, bool, error)
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
