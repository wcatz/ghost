package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/wcatz/ghost/internal/ai"
	"github.com/wcatz/ghost/internal/memory"
)

type memorySearchInput struct {
	Query    string `json:"query"`
	Category string `json:"category"`
	Limit    int    `json:"limit"`
}

func registerMemorySearch(r *Registry, store *memory.Store) {
	r.Register(
		ai.ToolDefinition{
			Name:        "memory_search",
			Description: "Search project memories by keyword. Returns memories ranked by relevance and importance.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"query":    map[string]interface{}{"type": "string", "description": "Search query for memories"},
					"category": map[string]interface{}{"type": "string", "description": "Filter by category (optional)"},
					"limit":    map[string]interface{}{"type": "integer", "description": "Max results (default: 10)"},
				},
				"required": []string{"query"},
			},
		},
		makeMemorySearchExec(store),
		ApprovalNone,
	)
}

func makeMemorySearchExec(store *memory.Store) Executor {
	return func(ctx context.Context, projectPath string, input json.RawMessage) Result {
		var in memorySearchInput
		if err := json.Unmarshal(input, &in); err != nil {
			return Result{Content: fmt.Sprintf("invalid input: %v", err), IsError: true}
		}

		limit := in.Limit
		if limit <= 0 {
			limit = 10
		}

		projectID := projectIDFromPath(projectPath)

		var memories []memory.Memory
		var err error

		if in.Category != "" {
			memories, err = store.GetByCategory(ctx, projectID, in.Category, limit)
		} else {
			memories, err = store.SearchFTS(ctx, projectID, in.Query, limit)
		}

		if err != nil {
			return Result{Content: fmt.Sprintf("search failed: %v", err), IsError: true}
		}

		if len(memories) == 0 {
			return Result{Content: "no memories found"}
		}

		var sb strings.Builder
		for _, m := range memories {
			pin := ""
			if m.Pinned {
				pin = " [pinned]"
			}
			sb.WriteString(fmt.Sprintf("- [%s] (imp:%.1f, src:%s%s) %s\n", m.Category, m.Importance, m.Source, pin, m.Content))
		}

		return Result{Content: sb.String()}
	}
}
