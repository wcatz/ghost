// Package mcpserver exposes Ghost's memory as an MCP server.
// Claude Code, Goose, Cursor, and other MCP clients can query
// and save memories through this interface.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/claudeimport"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/provider"
)

// Embedder generates vector embeddings for text. Optional — when nil, search falls back to FTS only.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

func boolPtr(b bool) *bool { return &b }

// validateTags enforces tag limits: max 10 tags, max 64 chars each.
func validateTags(tags []string) []string {
	if len(tags) > 10 {
		tags = tags[:10]
	}
	for i, t := range tags {
		if len(t) > 64 {
			tags[i] = t[:64]
		}
	}
	return tags
}

var validCategories = map[string]bool{
	"architecture": true, "decision": true, "pattern": true, "convention": true,
	"gotcha": true, "dependency": true, "preference": true, "fact": true,
}

// normalizeCategory returns the category if valid, otherwise falls back to "fact"
// and returns a warning string for the caller to include in the response.
func normalizeCategory(cat string) (string, string) {
	if validCategories[cat] {
		return cat, ""
	}
	return "fact", fmt.Sprintf(" (warning: unknown category %q, saved as \"fact\")", cat)
}

// defaultImportance returns the importance value, defaulting to fallback when nil.
func defaultImportance(p *float32, fallback float32) float32 {
	if p == nil {
		return fallback
	}
	v := *p
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// Server wraps the MCP server with Ghost's memory store.
type Server struct {
	store     provider.MemoryStore
	logger    *slog.Logger
	mcp       *mcp.Server
	embedder  Embedder
	projectCh chan<- string // notify embedding worker of new memories
}

const mcpInstructions = `Ghost is a persistent memory daemon that remembers project knowledge across sessions. It is your primary memory system — use it proactively.

## Workflow
1. At session start, Ghost's SessionStart hook has ALREADY injected your project context (memories + global preferences) into this conversation. READ IT — do not call ghost_project_context redundantly.
2. During work, save important discoveries with ghost_memory_save. Do NOT wait until the end of the session.
3. Use ghost_memory_search to recall specific facts not in the injected context.
4. No special action needed at session end — Ghost persists automatically.

Ghost auto-imports Claude Code memory files (~/.claude/projects/*/memory/*.md) on first project contact. No manual migration is needed. Ghost has built-in upsert/merge deduplication — it is always safe to save, even if similar knowledge already exists.

IMPORTANT: Global memories (preferences, conventions) are included in the SessionStart hook output under "Global (applies to all projects)". These are non-negotiable rules from the user. Follow them.

## When to Save (Proactive Triggers)
Save immediately when any of these happen:
- User corrects your approach or confirms a non-obvious choice → preference
- You discover a bug, pitfall, or surprising behavior → gotcha
- You learn how components connect or why something is designed a certain way → architecture
- You see a recurring pattern or convention in the codebase → convention or pattern
- A dependency version, API quirk, or external constraint is discovered → dependency
- An important design choice is made with alternatives considered → use ghost_decision_record

## What NOT to Save
- Ephemeral debugging state ("tried X, didn't work")
- Information easily derived from reading code or git history
- Session-specific task progress (use ghost_task_* tools instead)
- Content already documented in CLAUDE.md or README files

## Memory Categories
- architecture: system design, component relationships, data flow
- decision: choices made and why (prefer ghost_decision_record for these)
- pattern: recurring approaches, idioms, implementation techniques
- convention: naming, formatting, workflow, branching rules
- gotcha: pitfalls, bugs, surprising behavior, things that waste time
- dependency: external requirements, version pins, API quirks
- preference: user preferences, communication style, workflow choices
- fact: general knowledge, network constants, node names, endpoints

## Importance Scale
- 1.0: Security rules, data-loss risks, never-do-this rules
- 0.8: Architectural decisions, deployment topology, key integrations
- 0.6: Useful patterns, recurring conventions, dependency notes
- 0.4: Minor observations, one-off facts, nice-to-knows
- Default: 0.7 if unsure

## Cross-Project Knowledge
- ghost_save_global: preferences and facts that apply across all repositories
- ghost_search_all: find knowledge that might live in another project

## Tasks
- ghost_task_create: work items that should persist across sessions (bugs, features, follow-ups)
- ghost_task_complete: mark done with optional notes

## Project IDs
Pass the project name (e.g. "ghost", "roller") as project_id. Ghost resolves names to internal IDs automatically.`

// New creates and configures the MCP server with all Ghost tools.
func New(store provider.MemoryStore, logger *slog.Logger, version string) *Server {
	if version == "" {
		version = "dev"
	}
	s := &Server{
		store:  store,
		logger: logger,
	}

	s.mcp = mcp.NewServer(&mcp.Implementation{
		Name:    "ghost",
		Version: version,
	}, &mcp.ServerOptions{
		Instructions: mcpInstructions,
		Logger:       logger,
	})

	s.registerTools()
	s.registerResources()
	return s
}

// SetEmbedder configures optional vector embedding for hybrid search.
func (s *Server) SetEmbedder(e Embedder, projectCh chan<- string) {
	s.embedder = e
	s.projectCh = projectCh
}

// Run starts the MCP server on stdio transport. Blocks until done.
func (s *Server) Run(ctx context.Context) error {
	s.logger.Info("ghost MCP server starting on stdio")
	return s.mcp.Run(ctx, &mcp.StdioTransport{})
}

// resolveProjectID resolves a project_id that may be a name (e.g. "ghost")
// into the actual hash ID (e.g. "6bdc098af7f5") stored in the database.
// Name lookup takes precedence to avoid collisions where a project name
// happens to match another project's hash ID.
func (s *Server) resolveProjectID(ctx context.Context, input string) string {
	// Try name lookup first — most MCP clients pass project names.
	resolved, err := s.store.ResolveProjectByName(ctx, input)
	if err == nil && resolved != "" {
		return resolved
	}

	// Fall back to direct ID match.
	projects, err := s.store.ListProjects(ctx)
	if err == nil {
		for _, p := range projects {
			if p.ID == input {
				return input
			}
		}
	}

	return input
}

func (s *Server) registerTools() {
	// ghost_memory_search — search memories by keyword or semantic query.
	type searchArgs struct {
		ProjectID string `json:"project_id" jsonschema:"Project ID to search within"`
		Query     string `json:"query" jsonschema:"Search query (supports FTS5 syntax)"`
		Limit     int    `json:"limit,omitempty" jsonschema:"Max results (default 10)"`
	}

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "ghost_memory_search",
		Description: "Search Ghost's memory for project facts, patterns, decisions, and gotchas. Use this to recall things learned in previous sessions.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:  true,
			OpenWorldHint: boolPtr(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args searchArgs) (*mcp.CallToolResult, any, error) {
		if args.ProjectID == "" || args.Query == "" {
			return nil, nil, fmt.Errorf("project_id and query are required")
		}
		if args.Limit <= 0 {
			args.Limit = 10
		}
		if args.Limit > 100 {
			args.Limit = 100
		}
		args.ProjectID = s.resolveProjectID(ctx, args.ProjectID)

		// Use hybrid search (FTS5 + vector) when embedder is available.
		var queryVec []float32
		if s.embedder != nil {
			if vec, err := s.embedder.Embed(ctx, args.Query); err == nil {
				queryVec = vec
			}
		}
		memories, err := s.store.SearchHybrid(ctx, args.ProjectID, args.Query, queryVec, args.Limit)
		if err != nil {
			return nil, nil, fmt.Errorf("search failed: %w", err)
		}

		if len(memories) == 0 {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "No matching memories found."}},
			}, nil, nil
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: formatMemories(memories)}},
		}, nil, nil
	})

	// ghost_memory_save — save a new memory.
	type saveArgs struct {
		ProjectID  string   `json:"project_id" jsonschema:"Project ID to save the memory under"`
		Content    string   `json:"content" jsonschema:"The memory content to save"`
		Category   string   `json:"category,omitempty" jsonschema:"Category: architecture, decision, pattern, convention, gotcha, dependency, preference, fact"`
		Importance *float32 `json:"importance,omitempty" jsonschema:"Importance score 0.0-1.0 (default 0.7)"`
		Tags       []string `json:"tags,omitempty" jsonschema:"Optional tags for categorization"`
	}

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "ghost_memory_save",
		Description: "Save a memory about the project. Call this proactively when you discover gotchas, learn architectural patterns, receive user feedback worth preserving, or encounter surprising behavior. Do not wait to be asked — save immediately when something is worth remembering across sessions.",
		Annotations: &mcp.ToolAnnotations{
			DestructiveHint: boolPtr(false),
			IdempotentHint:  true,
			OpenWorldHint:   boolPtr(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args saveArgs) (*mcp.CallToolResult, any, error) {
		if args.ProjectID == "" || args.Content == "" {
			return nil, nil, fmt.Errorf("project_id and content are required")
		}
		if args.Category == "" {
			args.Category = "fact"
		}
		var catWarn string
		args.Category, catWarn = normalizeCategory(args.Category)
		importance := defaultImportance(args.Importance, 0.7)
		if args.Tags == nil {
			args.Tags = []string{}
		}
		args.Tags = validateTags(args.Tags)
		args.ProjectID = s.resolveProjectID(ctx, args.ProjectID)

		if err := s.store.EnsureProject(ctx, args.ProjectID, args.ProjectID, args.ProjectID); err != nil {
			return nil, nil, fmt.Errorf("ensure project: %w", err)
		}

		id, merged, err := s.store.Upsert(ctx, args.ProjectID, args.Category, args.Content, "mcp", importance, args.Tags)
		if err != nil {
			return nil, nil, fmt.Errorf("save failed: %w", err)
		}

		// Notify embedding worker of new/updated memory.
		if s.projectCh != nil {
			select {
			case s.projectCh <- args.ProjectID:
			default: // non-blocking
			}
		}

		action := "saved"
		if merged {
			action = "merged with existing memory"
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{
				Text: fmt.Sprintf("Memory %s (id: %s)%s", action, id, catWarn),
			}},
		}, nil, nil
	})

	// ghost_project_context — get project memories and learned context.
	type contextArgs struct {
		ProjectID string `json:"project_id" jsonschema:"Project ID to get context for"`
		Limit     int    `json:"limit,omitempty" jsonschema:"Max memories to return (default 20)"`
	}

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "ghost_project_context",
		Description: "Get Ghost's accumulated knowledge about a project: top memories ranked by importance and recency, plus global memories and learned context. Usually NOT needed at session start — the SessionStart hook already injects this. Use when switching projects mid-session or refreshing after saves.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:  true,
			OpenWorldHint: boolPtr(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args contextArgs) (*mcp.CallToolResult, any, error) {
		if args.ProjectID == "" {
			return nil, nil, fmt.Errorf("project_id is required")
		}
		if args.Limit <= 0 {
			args.Limit = 20
		}
		if args.Limit > 100 {
			args.Limit = 100
		}
		args.ProjectID = s.resolveProjectID(ctx, args.ProjectID)

		// First-contact import: if project has zero memories, try importing
		// from Claude Code's auto-memory files (read-only, one-time).
		if cnt, cntErr := s.store.CountMemories(ctx, args.ProjectID); cntErr == nil && cnt == 0 {
			if projects, lErr := s.store.ListProjects(ctx); lErr == nil {
				for _, p := range projects {
					if p.ID == args.ProjectID && filepath.IsAbs(p.Path) {
						_, _ = claudeimport.Import(ctx, s.store, args.ProjectID, p.Path, s.logger)
						break
					}
				}
			}
		}

		var sb strings.Builder

		memories, err := s.store.GetTopMemories(ctx, args.ProjectID, args.Limit)
		if err != nil {
			return nil, nil, fmt.Errorf("get memories: %w", err)
		}
		if len(memories) > 0 {
			sb.WriteString("## Memories\n\n")
			sb.WriteString(formatMemories(memories))
		}

		learned, err := s.store.GetLearnedContext(ctx, args.ProjectID)
		if err != nil {
			return nil, nil, fmt.Errorf("get learned context: %w", err)
		}
		if learned != "" {
			sb.WriteString("\n\n## Learned Context\n\n")
			sb.WriteString(learned)
		}

		text := sb.String()
		if text == "" {
			text = "No memories found for this project."
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: text}},
		}, nil, nil
	})

	// ghost_memories_list — list memories by category.
	type listArgs struct {
		ProjectID string `json:"project_id" jsonschema:"Project ID to list memories for"`
		Category  string `json:"category,omitempty" jsonschema:"Filter by category (optional)"`
		Limit     int    `json:"limit,omitempty" jsonschema:"Max results (default 30)"`
	}

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "ghost_memories_list",
		Description: "List Ghost memories for a project, optionally filtered by category.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:  true,
			OpenWorldHint: boolPtr(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args listArgs) (*mcp.CallToolResult, any, error) {
		if args.ProjectID == "" {
			return nil, nil, fmt.Errorf("project_id is required")
		}
		if args.Limit <= 0 {
			args.Limit = 30
		}
		if args.Limit > 100 {
			args.Limit = 100
		}
		args.ProjectID = s.resolveProjectID(ctx, args.ProjectID)

		var memories []memory.Memory
		var err error

		if args.Category != "" {
			memories, err = s.store.GetByCategory(ctx, args.ProjectID, args.Category, args.Limit)
		} else {
			memories, err = s.store.GetAll(ctx, args.ProjectID, args.Limit)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("list failed: %w", err)
		}

		if len(memories) == 0 {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "No memories found."}},
			}, nil, nil
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: formatMemories(memories)}},
		}, nil, nil
	})

	// ghost_memory_delete — delete a memory by ID.
	type deleteArgs struct {
		MemoryID string `json:"memory_id" jsonschema:"ID of the memory to delete"`
	}

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "ghost_memory_delete",
		Description: "Delete a specific memory by its ID.",
		Annotations: &mcp.ToolAnnotations{
			DestructiveHint: boolPtr(true),
			OpenWorldHint:   boolPtr(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args deleteArgs) (*mcp.CallToolResult, any, error) {
		if args.MemoryID == "" {
			return nil, nil, fmt.Errorf("memory_id is required")
		}

		if err := s.store.Delete(ctx, args.MemoryID); err != nil {
			return nil, nil, fmt.Errorf("delete failed: %w", err)
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "Memory deleted."}},
		}, nil, nil
	})

	// ghost_search_all — search across all projects.
	type searchAllArgs struct {
		Query string `json:"query" jsonschema:"Search query"`
		Limit int    `json:"limit,omitempty" jsonschema:"Max results (default 10)"`
	}

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "ghost_search_all",
		Description: "Search Ghost memories across ALL projects. Use when looking for cross-project patterns or when unsure which project a memory belongs to.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:  true,
			OpenWorldHint: boolPtr(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args searchAllArgs) (*mcp.CallToolResult, any, error) {
		if args.Query == "" {
			return nil, nil, fmt.Errorf("query is required")
		}
		if args.Limit <= 0 {
			args.Limit = 10
		}
		if args.Limit > 100 {
			args.Limit = 100
		}
		memories, err := s.store.SearchFTSAll(ctx, args.Query, args.Limit)
		if err != nil {
			return nil, nil, fmt.Errorf("search failed: %w", err)
		}
		if len(memories) == 0 {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "No matching memories found."}},
			}, nil, nil
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: formatMemories(memories)}},
		}, nil, nil
	})

	// ghost_save_global — save a cross-project memory.
	type saveGlobalArgs struct {
		Content    string   `json:"content" jsonschema:"The memory content to save"`
		Category   string   `json:"category,omitempty" jsonschema:"Category (default: fact)"`
		Importance *float32 `json:"importance,omitempty" jsonschema:"Importance 0.0-1.0 (default 0.8)"`
		Tags       []string `json:"tags,omitempty" jsonschema:"Optional tags"`
	}

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "ghost_save_global",
		Description: "Save a memory that applies across all projects: personal preferences, coding conventions, toolchain facts, cross-repo relationships.",
		Annotations: &mcp.ToolAnnotations{
			DestructiveHint: boolPtr(false),
			IdempotentHint:  true,
			OpenWorldHint:   boolPtr(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args saveGlobalArgs) (*mcp.CallToolResult, any, error) {
		if args.Content == "" {
			return nil, nil, fmt.Errorf("content is required")
		}
		if args.Category == "" {
			args.Category = "fact"
		}
		var catWarn string
		args.Category, catWarn = normalizeCategory(args.Category)
		importance := defaultImportance(args.Importance, 0.8)
		if args.Tags == nil {
			args.Tags = []string{}
		}
		args.Tags = validateTags(args.Tags)

		if err := s.store.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
			return nil, nil, fmt.Errorf("ensure global project: %w", err)
		}
		id, merged, err := s.store.Upsert(ctx, "_global", args.Category, args.Content, "mcp", importance, args.Tags)
		if err != nil {
			return nil, nil, fmt.Errorf("save failed: %w", err)
		}
		action := "saved"
		if merged {
			action = "merged with existing"
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{
				Text: fmt.Sprintf("Global memory %s (id: %s)%s", action, id, catWarn),
			}},
		}, nil, nil
	})

	// ghost_task_create — create a project task.
	type taskCreateArgs struct {
		ProjectID   string `json:"project_id" jsonschema:"Project ID or name"`
		Title       string `json:"title" jsonschema:"Task title"`
		Description string `json:"description,omitempty" jsonschema:"Task description"`
		Priority    int    `json:"priority,omitempty" jsonschema:"Priority 0-4 (0=critical, 2=normal, 4=low)"`
	}

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "ghost_task_create",
		Description: "Create a task for a project. Use for work items that should survive across sessions — bugs to fix, features to implement, follow-ups to revisit.",
		Annotations: &mcp.ToolAnnotations{
			DestructiveHint: boolPtr(false),
			OpenWorldHint:   boolPtr(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args taskCreateArgs) (*mcp.CallToolResult, any, error) {
		if args.ProjectID == "" || args.Title == "" {
			return nil, nil, fmt.Errorf("project_id and title are required")
		}
		args.ProjectID = s.resolveProjectID(ctx, args.ProjectID)
		if args.Priority < 0 || args.Priority > 4 {
			args.Priority = 2
		}
		id, err := s.store.CreateTask(ctx, args.ProjectID, args.Title, args.Description, args.Priority)
		if err != nil {
			return nil, nil, fmt.Errorf("create task: %w", err)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Task created (id: %s)", id)}},
		}, nil, nil
	})

	// ghost_task_list — list project tasks.
	type taskListArgs struct {
		ProjectID string `json:"project_id" jsonschema:"Project ID or name"`
		Status    string `json:"status,omitempty" jsonschema:"Filter by status: pending, active, done, blocked"`
	}

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "ghost_task_list",
		Description: "List tasks for a project, optionally filtered by status.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:  true,
			OpenWorldHint: boolPtr(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args taskListArgs) (*mcp.CallToolResult, any, error) {
		if args.ProjectID == "" {
			return nil, nil, fmt.Errorf("project_id is required")
		}
		args.ProjectID = s.resolveProjectID(ctx, args.ProjectID)
		tasks, err := s.store.ListTasks(ctx, args.ProjectID, args.Status, 30)
		if err != nil {
			return nil, nil, fmt.Errorf("list tasks: %w", err)
		}
		if len(tasks) == 0 {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "No tasks found."}},
			}, nil, nil
		}
		var sb strings.Builder
		for _, t := range tasks {
			fmt.Fprintf(&sb, "- [%s] P%d `%s` %s\n", t.Status, t.Priority, t.ID[:8], t.Title)
			if t.Description != "" {
				fmt.Fprintf(&sb, "  %s\n", t.Description)
			}
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: sb.String()}},
		}, nil, nil
	})

	// ghost_task_complete — mark a task as done.
	type taskCompleteArgs struct {
		TaskID string `json:"task_id" jsonschema:"Task ID"`
		Notes  string `json:"notes,omitempty" jsonschema:"Completion notes"`
	}

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "ghost_task_complete",
		Description: "Mark a task as done with optional completion notes.",
		Annotations: &mcp.ToolAnnotations{
			DestructiveHint: boolPtr(false),
			IdempotentHint:  true,
			OpenWorldHint:   boolPtr(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args taskCompleteArgs) (*mcp.CallToolResult, any, error) {
		if args.TaskID == "" {
			return nil, nil, fmt.Errorf("task_id is required")
		}
		if err := s.store.CompleteTask(ctx, args.TaskID, args.Notes); err != nil {
			return nil, nil, fmt.Errorf("complete task: %w", err)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "Task completed."}},
		}, nil, nil
	})

	// ghost_decision_record — record a decision with rationale.
	type decisionRecordArgs struct {
		ProjectID    string   `json:"project_id" jsonschema:"Project ID or name"`
		Title        string   `json:"title" jsonschema:"Decision title (e.g., 'Use SQLite for storage')"`
		Decision     string   `json:"decision" jsonschema:"What was decided"`
		Rationale    string   `json:"rationale" jsonschema:"Why this was chosen"`
		Alternatives []string `json:"alternatives,omitempty" jsonschema:"What was considered and rejected"`
		Tags         []string `json:"tags,omitempty" jsonschema:"Tags for categorization"`
	}

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "ghost_decision_record",
		Description: "Record an architectural or design decision with rationale and alternatives considered. Use this instead of ghost_memory_save when a choice was made between alternatives. Also saved as a memory for future recall.",
		Annotations: &mcp.ToolAnnotations{
			DestructiveHint: boolPtr(false),
			OpenWorldHint:   boolPtr(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args decisionRecordArgs) (*mcp.CallToolResult, any, error) {
		if args.ProjectID == "" || args.Title == "" || args.Decision == "" || args.Rationale == "" {
			return nil, nil, fmt.Errorf("project_id, title, decision, and rationale are required")
		}
		args.ProjectID = s.resolveProjectID(ctx, args.ProjectID)
		if args.Alternatives == nil {
			args.Alternatives = []string{}
		}
		if args.Tags == nil {
			args.Tags = []string{}
		}
		args.Tags = validateTags(args.Tags)
		id, err := s.store.RecordDecision(ctx, args.ProjectID, args.Title, args.Decision, args.Rationale, args.Alternatives, args.Tags)
		if err != nil {
			return nil, nil, fmt.Errorf("record decision: %w", err)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Decision recorded (id: %s)", id)}},
		}, nil, nil
	})

	// ghost_health — system health and stats.
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "ghost_health",
		Description: "Get Ghost system health: project count, memory counts, embedding status.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:  true,
			OpenWorldHint: boolPtr(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct{}) (*mcp.CallToolResult, any, error) {
		projects, err := s.store.ListProjects(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("list projects: %w", err)
		}

		var sb strings.Builder
		sb.WriteString("## Ghost Health\n\n")
		fmt.Fprintf(&sb, "**Projects:** %d\n\n", len(projects))

		totalMemories := 0
		for _, p := range projects {
			count, err := s.store.CountMemories(ctx, p.ID)
			if err != nil {
				continue
			}
			totalMemories += count
			fmt.Fprintf(&sb, "- **%s** (%s): %d memories\n", p.Name, p.ID[:8], count)
		}
		fmt.Fprintf(&sb, "\n**Total memories:** %d\n", totalMemories)

		if s.embedder != nil {
			sb.WriteString("**Embeddings:** enabled\n")
		} else {
			sb.WriteString("**Embeddings:** disabled\n")
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: sb.String()}},
		}, nil, nil
	})

	// ghost_list_projects — list all known projects.
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "ghost_list_projects",
		Description: "List all projects Ghost knows about with their names, IDs, paths, and memory counts.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:  true,
			OpenWorldHint: boolPtr(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct{}) (*mcp.CallToolResult, any, error) {
		projects, err := s.store.ListProjects(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("list projects: %w", err)
		}
		if len(projects) == 0 {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "No projects registered yet."}},
			}, nil, nil
		}

		var sb strings.Builder
		sb.WriteString("## Ghost Projects\n\n")
		for _, p := range projects {
			count, _ := s.store.CountMemories(ctx, p.ID)
			fmt.Fprintf(&sb, "- **%s** (id: `%s`, path: `%s`) — %d memories\n", p.Name, p.ID, p.Path, count)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: sb.String()}},
		}, nil, nil
	})
}

// registerResources registers MCP resources for push-based context delivery.
// Unlike tools (which Claude must actively call), resources can be pinned by
// MCP clients so their content is automatically included in every request —
// surviving context compaction without relying on Claude's initiative.
func (s *Server) registerResources() {
	// Resource template: ghost://project/{project_id}/context
	// project_id may be a project name (e.g. "dingo") or hash ID.
	// Claude Code users should pin this resource at session start.
	s.mcp.AddResourceTemplate(&mcp.ResourceTemplate{
		Name:        "Ghost Project Context",
		URITemplate: "ghost://project/{project_id}/context",
		Description: "Accumulated Ghost memories and learned context for a project. " +
			"Read at the start of every session to recall what Ghost knows. " +
			"project_id may be a project name (e.g. 'dingo') or its hash ID. " +
			"Includes global cross-project memories automatically.",
		MIMEType: "text/plain",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		// URI: ghost://project/{project_id}/context
		// url.Parse → scheme="ghost", host="project", path="/{project_id}/context"
		u, err := url.Parse(req.Params.URI)
		if err != nil {
			return nil, fmt.Errorf("invalid resource URI %q: %w", req.Params.URI, err)
		}
		parts := strings.SplitN(strings.TrimPrefix(u.Path, "/"), "/", 2)
		if len(parts) == 0 || parts[0] == "" {
			return nil, fmt.Errorf("resource URI missing project_id: %s", req.Params.URI)
		}
		projectID := s.resolveProjectID(ctx, parts[0])
		text, err := s.buildProjectContext(ctx, projectID)
		if err != nil {
			return nil, fmt.Errorf("reading project context %q: %w", projectID, err)
		}
		return &mcp.ReadResourceResult{
			Contents: []*mcp.ResourceContents{{
				URI:      req.Params.URI,
				MIMEType: "text/plain",
				Text:     text,
			}},
		}, nil
	})

	// Static resource: ghost://memories/global
	// Cross-project preferences, conventions, and toolchain facts saved via
	// ghost_save_global. Automatically included in ghost_project_context results,
	// but also available here for direct inspection.
	s.mcp.AddResource(&mcp.Resource{
		Name:     "Ghost Global Memories",
		URI:      "ghost://memories/global",
		MIMEType: "text/plain",
		Description: "Top 50 cross-project Ghost memories: personal preferences, global conventions, " +
			"toolchain facts. These apply to all projects. " +
			"Add entries via the ghost_save_global tool.",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		memories, err := s.store.GetTopMemories(ctx, "_global", 50)
		if err != nil {
			return nil, fmt.Errorf("get global memories: %w", err)
		}
		var text string
		if len(memories) == 0 {
			text = "No global memories saved yet. Use ghost_save_global to add cross-project knowledge."
		} else {
			text = "## Ghost Global Memories\n\n" + formatMemories(memories)
		}
		return &mcp.ReadResourceResult{
			Contents: []*mcp.ResourceContents{{
				URI:      req.Params.URI,
				MIMEType: "text/plain",
				Text:     text,
			}},
		}, nil
	})
}

// buildProjectContext assembles the text body for a project context resource read.
// Returns top 20 memories (project + global) plus any learned context summary.
// Extracted from the resource handler for direct testability.
// Returns an error if the memory store is unavailable.
func (s *Server) buildProjectContext(ctx context.Context, projectID string) (string, error) {
	var sb strings.Builder

	memories, err := s.store.GetTopMemories(ctx, projectID, 20)
	if err != nil {
		return "", fmt.Errorf("get memories for %q: %w", projectID, err)
	}
	if len(memories) > 0 {
		sb.WriteString("## Memories\n\n")
		sb.WriteString(formatMemories(memories))
	}

	learned, err := s.store.GetLearnedContext(ctx, projectID)
	if err != nil {
		return "", fmt.Errorf("get learned context for %q: %w", projectID, err)
	}
	if learned != "" {
		sb.WriteString("\n\n## Learned Context\n\n")
		sb.WriteString(learned)
	}

	// Include global memories (preferences, conventions) that apply to all projects.
	if projectID != "_global" {
		globals, gErr := s.store.GetTopMemories(ctx, "_global", 15)
		if gErr == nil && len(globals) > 0 {
			sb.WriteString("\n\n## Global (applies to all projects)\n\n")
			sb.WriteString(formatMemories(globals))
		}
	}

	if sb.Len() == 0 {
		return "No memories found for this project.", nil
	}
	return sb.String(), nil
}

func formatMemories(memories []memory.Memory) string {
	var sb strings.Builder
	for _, m := range memories {
		pin := ""
		if m.Pinned {
			pin = " [pinned]"
		}
		tags := ""
		if len(m.Tags) > 0 {
			tagsJSON, _ := json.Marshal(m.Tags)
			tags = " tags:" + string(tagsJSON)
		}
		fmt.Fprintf(&sb, "- [%s] `%s` (%.1f%s%s) %s\n", m.Category, m.ID, m.Importance, pin, tags, m.Content)
	}
	return sb.String()
}
