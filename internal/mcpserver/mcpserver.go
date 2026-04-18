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

const mcpInstructions = `Ghost is your persistent memory system. It remembers project knowledge across sessions — use it proactively.

## Session Start
The SessionStart hook has ALREADY injected your project context into this conversation. It includes:
- The project name to use as project_id (shown in the "## Ghost context: {name}" heading)
- Top memories, open tasks, recent decisions, and global preferences
DO NOT call ghost_project_context redundantly — context is already loaded.

IMPORTANT: Global memories under "Global (applies to all projects)" are non-negotiable user rules. Follow them.

## When to Save
Save immediately with ghost_memory_save — do NOT batch or wait:
- User corrects you or confirms a non-obvious choice → category: preference
- Bug, pitfall, or surprising behavior discovered → category: gotcha
- Component relationships or design rationale learned → category: architecture
- Recurring pattern or convention observed → category: convention or pattern
- Dependency version, API quirk, or constraint found → category: dependency
- Design choice with alternatives → use ghost_decision_record instead

Do NOT save: ephemeral debug state, info derivable from code/git, content in CLAUDE.md.

## Cross-Project
When learning about project B while working in project A, pass project B's name as project_id.
Use ghost_save_global for preferences/facts that apply to ALL repos (not project-specific).
Use ghost_search_all to find knowledge that might be in another project.

## Project IDs
Pass the project name (e.g. "ghost", "roller", "infra") as project_id. Ghost resolves names automatically. Never pass raw filesystem paths.`

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

	// Fall back to direct ID match, then path match.
	projects, err := s.store.ListProjects(ctx)
	if err == nil {
		for _, p := range projects {
			if p.ID == input {
				return input
			}
		}
		// If input looks like an absolute path, match against project paths.
		// This prevents creating duplicate projects when Claude passes a raw
		// filesystem path instead of a project name or hash ID.
		if filepath.IsAbs(input) {
			for _, p := range projects {
				if p.Path == input {
					return p.ID
				}
			}
		}
	}

	return input
}

func (s *Server) registerTools() {
	// ghost_memory_search — search memories by keyword or semantic query.
	type searchArgs struct {
		ProjectID string `json:"project_id" jsonschema:"Project name (e.g. 'ghost', 'infra', 'roller')"`
		Query     string `json:"query" jsonschema:"Search query — natural language or FTS5 (e.g. 'helm deploy', 'sqlite*', 'auth AND token')"`
		Category  string `json:"category,omitempty" jsonschema:"Filter results to this category (optional)"`
		Limit     int    `json:"limit,omitempty" jsonschema:"Max results (default 10)"`
	}

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "ghost_memory_search",
		Description: "Search Ghost's memory for project facts, patterns, decisions, and gotchas. Use before making decisions, when encountering unfamiliar components, or when the user references prior work. Supports FTS5 queries (e.g. 'helm deploy', 'sqlite*'). When category is set, the search fetches limit*3 results then post-filters — results may be incomplete if that category is sparse in the index. For exhaustive category browsing use ghost_memories_list. Example: project_id='ghost', query='approval flow', category='architecture'.",
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
		searchLimit := args.Limit
		if args.Category != "" {
			searchLimit = args.Limit * 3
			if searchLimit > 100 {
				searchLimit = 100
			}
		}
		memories, err := s.store.SearchHybrid(ctx, args.ProjectID, args.Query, queryVec, searchLimit)
		if err != nil {
			return nil, nil, fmt.Errorf("search failed: %w", err)
		}

		// Post-filter by category if specified.
		if args.Category != "" {
			filtered := memories[:0]
			for _, m := range memories {
				if m.Category == args.Category {
					filtered = append(filtered, m)
				}
			}
			if len(filtered) > args.Limit {
				filtered = filtered[:args.Limit]
			}
			memories = filtered
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
		ProjectID  string   `json:"project_id" jsonschema:"Project name to save under (e.g. 'ghost'). Use the name from the session hook heading."`
		Content    string   `json:"content" jsonschema:"The memory content to save"`
		Category   string   `json:"category,omitempty" jsonschema:"architecture|decision|pattern|convention|gotcha|dependency|preference|fact (default: fact)"`
		Importance *float32 `json:"importance,omitempty" jsonschema:"Importance score 0.0-1.0 (default 0.7)"`
		Tags       []string `json:"tags,omitempty" jsonschema:"Optional tags for categorization"`
	}

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "ghost_memory_save",
		Description: "Save a memory about the project. Call proactively — do not wait to be asked. Write concise 1-3 sentence memories (truncated to ~300 chars in session context). Categories: architecture (system design), decision (choices made), pattern (recurring approaches), convention (naming/workflow), gotcha (pitfalls/bugs), dependency (versions/API quirks), preference (user preferences), fact (general knowledge). Importance: 1.0=security/never-do-this, 0.8=architecture/key decisions, 0.6=patterns/conventions, 0.4=minor observations, 0.7=default. Example: project_id='infra', content='k3s-mini-1 runs Grafana on port 80', category='fact', importance=0.7.",
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
		validCategories := map[string]bool{
			"architecture": true, "decision": true, "pattern": true, "convention": true,
			"gotcha": true, "dependency": true, "preference": true, "fact": true,
		}
		if !validCategories[args.Category] {
			return nil, nil, fmt.Errorf("invalid category %q — must be one of: architecture, decision, pattern, convention, gotcha, dependency, preference, fact", args.Category)
		}
		importance := defaultImportance(args.Importance, 0.7)
		if args.Tags == nil {
			args.Tags = []string{}
		}
		args.Tags = validateTags(args.Tags)
		args.ProjectID = s.resolveProjectID(ctx, args.ProjectID)

		const maxContentLen = 2000
		truncated := false
		if len(args.Content) > maxContentLen {
			args.Content = args.Content[:maxContentLen]
			truncated = true
		}

		// Pass "" for path — MCP callers don't have filesystem paths.
		if err := s.store.EnsureProject(ctx, args.ProjectID, "", args.ProjectID); err != nil {
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
		msg := fmt.Sprintf("Memory %s (id: %s)", action, id)
		if truncated {
			msg += fmt.Sprintf(" (content truncated to %d chars)", maxContentLen)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: msg}},
		}, nil, nil
	})

	// ghost_project_context — get project memories and learned context.
	type contextArgs struct {
		ProjectID string `json:"project_id" jsonschema:"Project name (e.g. 'ghost', 'infra')"`
		Limit     int    `json:"limit,omitempty" jsonschema:"Max memories to return (default 20)"`
	}

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "ghost_project_context",
		Description: "Get Ghost's accumulated knowledge about a project: top memories, global memories, and learned context. NOT needed at session start (hook already injects this). Use when switching projects mid-session or after saving 3+ memories to see updated context.",
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
		ProjectID string `json:"project_id" jsonschema:"Project name (e.g. 'ghost')"`
		Category  string `json:"category,omitempty" jsonschema:"Filter by category (optional)"`
		Limit     int    `json:"limit,omitempty" jsonschema:"Max results (default 30)"`
	}

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "ghost_memories_list",
		Description: "List Ghost memories for a project, optionally filtered by category. Use for browsing (e.g. 'show all gotchas') rather than keyword lookup — use ghost_memory_search for keyword queries.",
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
		ProjectID string `json:"project_id" jsonschema:"Project name the memory belongs to (required for ownership check)"`
		MemoryID  string `json:"memory_id" jsonschema:"ID of the memory to delete"`
	}

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "ghost_memory_delete",
		Description: "Permanently delete a memory by ID. Requires project_id to verify ownership — you cannot delete memories from other projects. Use only when the user explicitly asks to remove a memory or when a memory is confirmed incorrect. Do not delete outdated memories — Ghost's reflection system handles pruning.",
		Annotations: &mcp.ToolAnnotations{
			DestructiveHint: boolPtr(true),
			OpenWorldHint:   boolPtr(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args deleteArgs) (*mcp.CallToolResult, any, error) {
		if args.ProjectID == "" || args.MemoryID == "" {
			return nil, nil, fmt.Errorf("project_id and memory_id are required")
		}
		resolvedProjectID := s.resolveProjectID(ctx, args.ProjectID)

		// Verify the memory exists and belongs to the specified project.
		mems, err := s.store.GetByIDs(ctx, []string{args.MemoryID})
		if err != nil {
			return nil, nil, fmt.Errorf("lookup failed: %w", err)
		}
		if len(mems) == 0 {
			return nil, nil, fmt.Errorf("memory %s not found", args.MemoryID)
		}
		if mems[0].ProjectID != resolvedProjectID {
			return nil, nil, fmt.Errorf("memory %s does not belong to project %s", args.MemoryID, args.ProjectID)
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
		Description: "Search Ghost memories across ALL projects. Use when a pattern, dependency, or convention might be recorded under a different project, or when the user references knowledge from another repo.",
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

		var queryVec []float32
		if s.embedder != nil {
			if vec, err := s.embedder.Embed(ctx, args.Query); err == nil {
				queryVec = vec
			}
		}
		memories, err := s.store.SearchHybridAll(ctx, args.Query, queryVec, args.Limit)
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
		Description: "Save a cross-project memory: personal preferences, coding conventions, toolchain facts, cross-repo relationships. Use INSTEAD of ghost_memory_save when the knowledge is NOT specific to any single project. Example: content='Always use 2-space YAML indentation', category='convention'. WARNING: Global memories are injected into every project session as non-negotiable rules.",
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
		validCategories := map[string]bool{
			"architecture": true, "decision": true, "pattern": true, "convention": true,
			"gotcha": true, "dependency": true, "preference": true, "fact": true,
		}
		if !validCategories[args.Category] {
			return nil, nil, fmt.Errorf("invalid category %q — must be one of: architecture, decision, pattern, convention, gotcha, dependency, preference, fact", args.Category)
		}
		importance := defaultImportance(args.Importance, 0.8)
		if args.Tags == nil {
			args.Tags = []string{}
		}
		args.Tags = validateTags(args.Tags)

		const maxGlobalContentLen = 2000
		globalTruncated := false
		if len(args.Content) > maxGlobalContentLen {
			args.Content = args.Content[:maxGlobalContentLen]
			globalTruncated = true
		}

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
		msg := fmt.Sprintf("Global memory %s (id: %s)", action, id)
		if globalTruncated {
			msg += fmt.Sprintf(" (content truncated to %d chars)", maxGlobalContentLen)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: msg}},
		}, nil, nil
	})

	// ghost_task_create — create a project task.
	type taskCreateArgs struct {
		ProjectID   string `json:"project_id" jsonschema:"Project name (e.g. 'ghost')"`
		Title       string `json:"title" jsonschema:"Task title"`
		Description string `json:"description,omitempty" jsonschema:"Task description"`
		Priority    *int   `json:"priority,omitempty" jsonschema:"Priority 0-4 (0=critical, 2=normal, 4=low). Default: 2 (normal)"`
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
		priority := 2 // default: normal
		if args.Priority != nil {
			priority = *args.Priority
			if priority < 0 || priority > 4 {
				priority = 2
			}
		}
		id, err := s.store.CreateTask(ctx, args.ProjectID, args.Title, args.Description, priority)
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
		Limit     int    `json:"limit,omitempty" jsonschema:"Max results (default 30, max 100)"`
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
		if args.Limit <= 0 {
			args.Limit = 30
		}
		if args.Limit > 100 {
			args.Limit = 100
		}
		tasks, err := s.store.ListTasks(ctx, args.ProjectID, args.Status, args.Limit)
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
		Description: "Record an architectural or design decision with rationale and alternatives considered. Use instead of ghost_memory_save when a choice was made between alternatives. Also saved as a memory. Example: title='Use SQLite over Postgres', decision='Embedded SQLite with FTS5', rationale='Zero external deps, sufficient for single-user', alternatives=['PostgreSQL', 'Redis'].",
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
			fmt.Fprintf(&sb, "- **%s** (%s): %d memories\n", p.Name, p.ID[:min(len(p.ID), 8)], count)
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
		Description: "List all projects Ghost knows about with names, IDs, paths, and memory counts. Use to discover valid project_id values.",
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

	// ghost_memory_pin — pin or unpin a memory.
	type pinArgs struct {
		MemoryID string `json:"memory_id" jsonschema:"ID of the memory to pin/unpin"`
		Pinned   bool   `json:"pinned" jsonschema:"true to pin, false to unpin"`
	}

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "ghost_memory_pin",
		Description: "Pin or unpin a memory. Pinned memories always appear at top of project context and survive reflection pruning. Pin non-negotiable rules, security constraints, or core architectural invariants.",
		Annotations: &mcp.ToolAnnotations{
			DestructiveHint: boolPtr(false),
			IdempotentHint:  true,
			OpenWorldHint:   boolPtr(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args pinArgs) (*mcp.CallToolResult, any, error) {
		if args.MemoryID == "" {
			return nil, nil, fmt.Errorf("memory_id is required")
		}
		if err := s.store.TogglePin(ctx, args.MemoryID, args.Pinned); err != nil {
			return nil, nil, fmt.Errorf("toggle pin: %w", err)
		}
		action := "pinned"
		if !args.Pinned {
			action = "unpinned"
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Memory %s.", action)}},
		}, nil, nil
	})

	// ghost_task_update — update a task's status, priority, or description.
	// Priority and description are optional — omitting them preserves current values.
	type taskUpdateArgs struct {
		TaskID      string  `json:"task_id" jsonschema:"Task ID to update"`
		Status      string  `json:"status,omitempty" jsonschema:"New status: pending, active, blocked, done (omit to preserve current)"`
		Priority    *int    `json:"priority,omitempty" jsonschema:"Priority 0-4 (0=critical, 2=normal, 4=low). Omit to keep current value."`
		Description *string `json:"description,omitempty" jsonschema:"Updated description. Omit to keep current value."`
	}

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "ghost_task_update",
		Description: "Update a task's status, priority, or description. All fields are optional — omit any field to preserve its current value. Only pass what you want to change.",
		Annotations: &mcp.ToolAnnotations{
			DestructiveHint: boolPtr(false),
			IdempotentHint:  true,
			OpenWorldHint:   boolPtr(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args taskUpdateArgs) (*mcp.CallToolResult, any, error) {
		if args.TaskID == "" {
			return nil, nil, fmt.Errorf("task_id is required")
		}

		// Fetch current task to fill in omitted fields (prevents zero-value clobber).
		current, err := s.store.GetTask(ctx, args.TaskID)
		if err != nil {
			return nil, nil, fmt.Errorf("task not found: %w", err)
		}

		// status is optional — omitting it preserves the current value.
		status := current.Status
		if args.Status != "" {
			validStatuses := map[string]bool{
				"pending": true, "active": true, "blocked": true, "done": true,
			}
			if !validStatuses[args.Status] {
				return nil, nil, fmt.Errorf("invalid status %q — must be one of: pending, active, blocked, done", args.Status)
			}
			status = args.Status
		}

		priority := current.Priority
		if args.Priority != nil {
			priority = *args.Priority
			if priority < 0 || priority > 4 {
				priority = 2
			}
		}
		description := current.Description
		if args.Description != nil {
			description = *args.Description
		}

		if err := s.store.UpdateTask(ctx, args.TaskID, status, priority, description); err != nil {
			return nil, nil, fmt.Errorf("update task: %w", err)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "Task updated."}},
		}, nil, nil
	})

	// ghost_decisions_list — list recorded decisions for a project.
	type decisionsListArgs struct {
		ProjectID string `json:"project_id" jsonschema:"Project ID or name"`
		Status    string `json:"status,omitempty" jsonschema:"Filter by status: active, superseded, revisit (default: all)"`
		Limit     int    `json:"limit,omitempty" jsonschema:"Max results (default 20)"`
	}

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "ghost_decisions_list",
		Description: "List recorded decisions for a project. Before making an architectural decision, check if a prior decision already covers the same area. Shows rationale and rejected alternatives.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:  true,
			OpenWorldHint: boolPtr(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args decisionsListArgs) (*mcp.CallToolResult, any, error) {
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

		decisions, err := s.store.ListDecisions(ctx, args.ProjectID, args.Status, args.Limit)
		if err != nil {
			return nil, nil, fmt.Errorf("list decisions: %w", err)
		}
		if len(decisions) == 0 {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "No decisions found."}},
			}, nil, nil
		}

		var sb strings.Builder
		for _, d := range decisions {
			fmt.Fprintf(&sb, "### %s\n", d.Title)
			fmt.Fprintf(&sb, "**Decision:** %s\n", d.Decision)
			fmt.Fprintf(&sb, "**Rationale:** %s\n", d.Rationale)
			if len(d.Alternatives) > 0 {
				fmt.Fprintf(&sb, "**Alternatives rejected:** %s\n", strings.Join(d.Alternatives, ", "))
			}
			fmt.Fprintf(&sb, "**Status:** %s | **ID:** `%s`\n\n", d.Status, d.ID)
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
		rawID, err := parseProjectIDFromURI(req.Params.URI)
		if err != nil {
			return nil, err
		}
		projectID := s.resolveProjectID(ctx, rawID)
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
		memories, err := s.store.GetTopMemories(ctx, "_global", 15)
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

	// Resource template: ghost://project/{project_id}/decisions
	s.mcp.AddResourceTemplate(&mcp.ResourceTemplate{
		Name:        "Ghost Project Decisions",
		URITemplate: "ghost://project/{project_id}/decisions",
		Description: "Active architectural and design decisions for a project. " +
			"Pin this resource to keep decision context visible across compaction.",
		MIMEType: "text/plain",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		rawID, err := parseProjectIDFromURI(req.Params.URI)
		if err != nil {
			return nil, err
		}
		projectID := s.resolveProjectID(ctx, rawID)

		decisions, err := s.store.ListDecisions(ctx, projectID, "active", 20)
		if err != nil {
			return nil, fmt.Errorf("list decisions for %q: %w", projectID, err)
		}

		var text string
		if len(decisions) == 0 {
			text = "No active decisions for this project."
		} else {
			var sb strings.Builder
			sb.WriteString("## Active Decisions\n\n")
			for _, d := range decisions {
				fmt.Fprintf(&sb, "- **%s**: %s (rationale: %s)\n", d.Title, d.Decision, d.Rationale)
			}
			text = sb.String()
		}

		return &mcp.ReadResourceResult{
			Contents: []*mcp.ResourceContents{{
				URI:      req.Params.URI,
				MIMEType: "text/plain",
				Text:     text,
			}},
		}, nil
	})

	// Resource template: ghost://project/{project_id}/tasks
	s.mcp.AddResourceTemplate(&mcp.ResourceTemplate{
		Name:        "Ghost Project Tasks",
		URITemplate: "ghost://project/{project_id}/tasks",
		Description: "Open tasks (pending, active, blocked) for a project. " +
			"Pin this resource to keep task context visible across compaction.",
		MIMEType: "text/plain",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		rawID, err := parseProjectIDFromURI(req.Params.URI)
		if err != nil {
			return nil, err
		}
		projectID := s.resolveProjectID(ctx, rawID)

		var sb strings.Builder
		sb.WriteString("## Open Tasks\n\n")
		hasContent := false

		for _, status := range []string{"active", "blocked", "pending"} {
			tasks, err := s.store.ListTasks(ctx, projectID, status, 15)
			if err != nil {
				continue
			}
			for _, t := range tasks {
				hasContent = true
				fmt.Fprintf(&sb, "- [%s] P%d `%s` %s\n", t.Status, t.Priority, t.ID[:8], t.Title)
				if t.Description != "" {
					fmt.Fprintf(&sb, "  %s\n", t.Description)
				}
			}
		}

		var text string
		if !hasContent {
			text = "No open tasks for this project."
		} else {
			text = sb.String()
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

// parseProjectIDFromURI extracts and URL-decodes the project_id segment from
// a ghost:// resource URI (e.g. "ghost://project/my%20proj/context" → "my proj").
func parseProjectIDFromURI(rawURI string) (string, error) {
	u, err := url.Parse(rawURI)
	if err != nil {
		return "", fmt.Errorf("invalid resource URI %q: %w", rawURI, err)
	}
	parts := strings.SplitN(strings.TrimPrefix(u.Path, "/"), "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		return "", fmt.Errorf("resource URI missing project_id: %s", rawURI)
	}
	projectID, err := url.PathUnescape(parts[0])
	if err != nil {
		return "", fmt.Errorf("invalid project_id encoding in URI %q: %w", rawURI, err)
	}
	return projectID, nil
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
			tagsJSON, err := json.Marshal(m.Tags)
			if err == nil {
				tags = " tags:" + string(tagsJSON)
			}
		}
		fmt.Fprintf(&sb, "- [%s] `%s` (%.1f%s%s) %s\n", m.Category, m.ID, m.Importance, pin, tags, m.Content)
	}
	return sb.String()
}
