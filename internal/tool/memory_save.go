package tool

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/wcatz/ghost/internal/ai"
	"github.com/wcatz/ghost/internal/memory"
)

type memorySaveInput struct {
	Content    string   `json:"content"`
	Category   string   `json:"category"`
	Importance float32  `json:"importance"`
	Tags       []string `json:"tags"`
}

func registerMemorySave(r *Registry, store *memory.Store) {
	r.Register(
		ai.ToolDefinition{
			Name:        "memory_save",
			Description: "Save a memory about this project. Use this when you learn something important: architecture decisions, conventions, gotchas, patterns, or developer preferences.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"content":    map[string]interface{}{"type": "string", "description": "Memory content (1-2 sentences, specific and actionable)"},
					"category":   map[string]interface{}{"type": "string", "enum": []string{"architecture", "decision", "pattern", "convention", "gotcha", "dependency", "preference", "fact"}, "description": "Memory category"},
					"importance": map[string]interface{}{"type": "number", "description": "Importance score 0.0-1.0 (default: 0.5)"},
					"tags":       map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "1-3 keyword tags"},
				},
				"required": []string{"content", "category"},
			},
		},
		makeMemorySaveExec(store),
		ApprovalNone,
	)
}

func makeMemorySaveExec(store *memory.Store) Executor {
	return func(ctx context.Context, projectPath string, input json.RawMessage) Result {
		var in memorySaveInput
		if err := json.Unmarshal(input, &in); err != nil {
			return Result{Content: fmt.Sprintf("invalid input: %v", err), IsError: true}
		}

		if in.Importance <= 0 {
			in.Importance = 0.5
		}
		if in.Importance > 1.0 {
			in.Importance = 1.0
		}
		if in.Tags == nil {
			in.Tags = []string{}
		}

		// Project ID is derived from the project path — caller must set it in context.
		projectID := projectIDFromPath(projectPath)

		id, merged, err := store.Upsert(ctx, projectID, in.Category, in.Content, "tool", in.Importance, in.Tags)
		if err != nil {
			return Result{Content: fmt.Sprintf("save failed: %v", err), IsError: true}
		}

		if merged {
			return Result{Content: fmt.Sprintf("memory strengthened (merged with existing): %s", id)}
		}
		return Result{Content: fmt.Sprintf("memory saved: %s [%s] %.1f", id, in.Category, in.Importance)}
	}
}
