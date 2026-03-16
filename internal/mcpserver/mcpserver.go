// Package mcpserver exposes Ghost's memory as an MCP server.
// Claude Code, Goose, Cursor, and other MCP clients can query
// and save memories through this interface.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/provider"
)

// Embedder generates vector embeddings for text. Optional — when nil, search falls back to FTS only.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// Server wraps the MCP server with Ghost's memory store.
type Server struct {
	store     provider.MemoryStore
	logger    *slog.Logger
	mcp       *mcp.Server
	embedder  Embedder
	projectCh chan<- string // notify embedding worker of new memories
}

// New creates and configures the MCP server with all Ghost tools.
func New(store provider.MemoryStore, logger *slog.Logger) *Server {
	s := &Server{
		store:  store,
		logger: logger,
	}

	s.mcp = mcp.NewServer(&mcp.Implementation{
		Name:    "ghost",
		Version: "0.1.0",
	}, &mcp.ServerOptions{
		Instructions: "Ghost is a persistent memory daemon. Use these tools to search, save, and manage project memories across sessions.",
		Logger:       logger,
	})

	s.registerTools()
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
	}, func(ctx context.Context, req *mcp.CallToolRequest, args searchArgs) (*mcp.CallToolResult, any, error) {
		if args.ProjectID == "" || args.Query == "" {
			return nil, nil, fmt.Errorf("project_id and query are required")
		}
		if args.Limit <= 0 {
			args.Limit = 10
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
		Importance float32  `json:"importance,omitempty" jsonschema:"Importance score 0.0-1.0 (default 0.7)"`
		Tags       []string `json:"tags,omitempty" jsonschema:"Optional tags for categorization"`
	}

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "ghost_memory_save",
		Description: "Save a memory about the project. Use this when you learn something important: architectural decisions, gotchas, conventions, patterns, or facts worth remembering across sessions.",
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
		if args.Importance <= 0 {
			args.Importance = 0.7
		}
		if args.Importance > 1 {
			args.Importance = 1
		}
		if args.Tags == nil {
			args.Tags = []string{}
		}
		args.ProjectID = s.resolveProjectID(ctx, args.ProjectID)

		if err := s.store.EnsureProject(ctx, args.ProjectID, args.ProjectID, args.ProjectID); err != nil {
			return nil, nil, fmt.Errorf("ensure project: %w", err)
		}

		id, merged, err := s.store.Upsert(ctx, args.ProjectID, args.Category, args.Content, "mcp", args.Importance, args.Tags)
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
				Text: fmt.Sprintf("Memory %s (id: %s)", action, id),
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
		Description: "Get Ghost's accumulated knowledge about a project: top memories ranked by importance and recency, plus any learned context summaries. Use this at the start of a session to recall what Ghost knows.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args contextArgs) (*mcp.CallToolResult, any, error) {
		if args.ProjectID == "" {
			return nil, nil, fmt.Errorf("project_id is required")
		}
		if args.Limit <= 0 {
			args.Limit = 20
		}
		args.ProjectID = s.resolveProjectID(ctx, args.ProjectID)

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
	}, func(ctx context.Context, req *mcp.CallToolRequest, args listArgs) (*mcp.CallToolResult, any, error) {
		if args.ProjectID == "" {
			return nil, nil, fmt.Errorf("project_id is required")
		}
		if args.Limit <= 0 {
			args.Limit = 30
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
	}, func(ctx context.Context, req *mcp.CallToolRequest, args searchAllArgs) (*mcp.CallToolResult, any, error) {
		if args.Query == "" {
			return nil, nil, fmt.Errorf("query is required")
		}
		if args.Limit <= 0 {
			args.Limit = 10
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
		Importance float32  `json:"importance,omitempty" jsonschema:"Importance 0.0-1.0 (default 0.8)"`
		Tags       []string `json:"tags,omitempty" jsonschema:"Optional tags"`
	}

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "ghost_save_global",
		Description: "Save a memory that applies across all projects: personal preferences, coding conventions, toolchain facts, cross-repo relationships.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args saveGlobalArgs) (*mcp.CallToolResult, any, error) {
		if args.Content == "" {
			return nil, nil, fmt.Errorf("content is required")
		}
		if args.Category == "" {
			args.Category = "fact"
		}
		if args.Importance <= 0 {
			args.Importance = 0.8
		}
		if args.Importance > 1 {
			args.Importance = 1
		}
		if args.Tags == nil {
			args.Tags = []string{}
		}

		if err := s.store.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
			return nil, nil, fmt.Errorf("ensure global project: %w", err)
		}
		id, merged, err := s.store.Upsert(ctx, "_global", args.Category, args.Content, "mcp", args.Importance, args.Tags)
		if err != nil {
			return nil, nil, fmt.Errorf("save failed: %w", err)
		}
		action := "saved"
		if merged {
			action = "merged with existing"
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{
				Text: fmt.Sprintf("Global memory %s (id: %s)", action, id),
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
		Description: "Create a task for a project. Use to track planned work, bugs, or action items.",
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
			sb.WriteString(fmt.Sprintf("- [%s] P%d `%s` %s\n", t.Status, t.Priority, t.ID[:8], t.Title))
			if t.Description != "" {
				sb.WriteString(fmt.Sprintf("  %s\n", t.Description))
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
		Description: "Record an architectural or design decision with rationale and alternatives considered. Also saved as a memory for future recall.",
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
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct{}) (*mcp.CallToolResult, any, error) {
		projects, err := s.store.ListProjects(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("list projects: %w", err)
		}

		var sb strings.Builder
		sb.WriteString("## Ghost Health\n\n")
		sb.WriteString(fmt.Sprintf("**Projects:** %d\n\n", len(projects)))

		totalMemories := 0
		for _, p := range projects {
			count, err := s.store.CountMemories(ctx, p.ID)
			if err != nil {
				continue
			}
			totalMemories += count
			sb.WriteString(fmt.Sprintf("- **%s** (%s): %d memories\n", p.Name, p.ID[:8], count))
		}
		sb.WriteString(fmt.Sprintf("\n**Total memories:** %d\n", totalMemories))

		if s.embedder != nil {
			sb.WriteString("**Embeddings:** enabled\n")
		} else {
			sb.WriteString("**Embeddings:** disabled\n")
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: sb.String()}},
		}, nil, nil
	})
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
		sb.WriteString(fmt.Sprintf("- [%s] (%.1f%s%s) %s\n", m.Category, m.Importance, pin, tags, m.Content))
	}
	return sb.String()
}
