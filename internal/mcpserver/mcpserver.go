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

// Server wraps the MCP server with Ghost's memory store.
type Server struct {
	store  provider.MemoryStore
	logger *slog.Logger
	mcp    *mcp.Server
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

// Run starts the MCP server on stdio transport. Blocks until done.
func (s *Server) Run(ctx context.Context) error {
	s.logger.Info("ghost MCP server starting on stdio")
	return s.mcp.Run(ctx, &mcp.StdioTransport{})
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

		memories, err := s.store.SearchFTS(ctx, args.ProjectID, args.Query, args.Limit)
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
		if args.Importance <= 0 {
			args.Importance = 0.7
		}
		if args.Tags == nil {
			args.Tags = []string{}
		}

		id, merged, err := s.store.Upsert(ctx, args.ProjectID, args.Category, args.Content, "mcp", args.Importance, args.Tags)
		if err != nil {
			return nil, nil, fmt.Errorf("save failed: %w", err)
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

		var sb strings.Builder

		memories, err := s.store.GetTopMemories(ctx, args.ProjectID, args.Limit)
		if err == nil && len(memories) > 0 {
			sb.WriteString("## Memories\n\n")
			sb.WriteString(formatMemories(memories))
		}

		learned, err := s.store.GetLearnedContext(ctx, args.ProjectID)
		if err == nil && learned != "" {
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
