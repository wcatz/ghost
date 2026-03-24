package reflection

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/ai"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/project"
)

// reflector is the subset of provider.LLMProvider needed for reflection.
type reflector interface {
	Reflect(ctx context.Context, prompt string) (string, ai.TokenUsage, error)
}

// memoryStore is the subset of provider.MemoryStore needed for reflection.
type memoryStore interface {
	IncrementInteraction(ctx context.Context, projectID string) (int, error)
	GetRecentExchanges(ctx context.Context, projectID string, limit int) ([][2]string, error)
	GetAll(ctx context.Context, projectID string, limit int) ([]memory.Memory, error)
	GetLearnedContext(ctx context.Context, projectID string) (string, error)
	ReplaceNonManual(ctx context.Context, projectID string, memories []memory.Memory) error
	UpdateLearnedContext(ctx context.Context, projectID, learnedContext, summary string) error
	Upsert(ctx context.Context, projectID, category, content, source string, importance float32, tags []string) (string, bool, error)
}

// Engine manages periodic reflection for memory consolidation.
type Engine struct {
	consolidator Consolidator
	store        memoryStore
	logger       *slog.Logger
	interval     int
}

// NewEngine creates a reflection engine with a consolidator.
// If consolidator is nil, consolidation is disabled (MaybeReflect becomes a no-op).
func NewEngine(consolidator Consolidator, store memoryStore, logger *slog.Logger, interval int) *Engine {
	if interval <= 0 {
		interval = 10
	}
	return &Engine{
		consolidator: consolidator,
		store:        store,
		logger:       logger,
		interval:     interval,
	}
}

// MaybeReflect increments the interaction count and triggers reflection
// when the count hits the interval. Safe to call from a goroutine.
func (e *Engine) MaybeReflect(ctx context.Context, projectID string, projCtx *project.Context) {
	if e.consolidator == nil || e.store == nil {
		return
	}

	count, err := e.store.IncrementInteraction(ctx, projectID)
	if err != nil {
		e.logger.Error("increment interaction count", "error", err, "project_id", projectID)
		return
	}
	if count < e.interval || count%e.interval != 0 {
		return
	}

	e.logger.Info("reflection triggered", "project_id", projectID, "interaction_count", count)

	reflectCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Gather inputs.
	exchanges, err := e.store.GetRecentExchanges(reflectCtx, projectID, 15)
	if err != nil {
		e.logger.Error("get exchanges for reflection", "error", err)
		exchanges = nil
	}

	existingMemories, err := e.store.GetAll(reflectCtx, projectID, 200)
	if err != nil {
		e.logger.Error("get memories for reflection", "error", err)
		existingMemories = nil
	}

	currentContext, _ := e.store.GetLearnedContext(reflectCtx, projectID)

	input := ReflectionInput{
		RecentExchanges:  exchanges,
		ExistingMemories: existingMemories,
		CurrentContext:   currentContext,
		LastCommits:      projCtx.LastCommits,
		ProjectLanguage:  projCtx.Language,
		ProjectName:      projCtx.Name,
	}

	result, err := e.consolidator.Consolidate(reflectCtx, input)
	if err != nil {
		e.logger.Error("consolidation failed", "error", err, "project_id", projectID, "tier", e.consolidator.Name())
		return
	}

	// Filter out empty-content memories before processing.
	var validMemories []ReflectMemory
	for _, m := range result.Memories {
		if strings.TrimSpace(m.Content) != "" {
			validMemories = append(validMemories, m)
		}
	}
	result.Memories = validMemories

	if len(result.Memories) > 0 {
		// Guard against dramatic reduction — count existing non-manual memories.
		var existingNonManual int
		for _, m := range existingMemories {
			if m.Source != "manual" {
				existingNonManual++
			}
		}
		if existingNonManual >= 6 && len(result.Memories) < existingNonManual/2 {
			e.logger.Warn("reflection returned too few memories — skipping replace to prevent data loss",
				"project_id", projectID,
				"existing_non_manual", existingNonManual,
				"reflection_returned", len(result.Memories),
			)
			return
		}

		dbMemories := make([]memory.Memory, len(result.Memories))
		for i, m := range result.Memories {
			dbMemories[i] = memory.Memory{
				ProjectID:  projectID,
				Category:   m.Category,
				Content:    m.Content,
				Importance: m.Importance,
				Source:     "reflection",
				Tags:       m.Tags,
			}
		}
		if err := e.store.ReplaceNonManual(reflectCtx, projectID, dbMemories); err != nil {
			e.logger.Error("save ghost memories", "error", err, "project_id", projectID)
		} else {
			e.logger.Info("ghost memories updated", "project_id", projectID, "count", len(dbMemories))

			// Build summary.
			catCounts := make(map[string]int)
			for _, m := range dbMemories {
				catCounts[m.Category]++
			}
			var parts []string
			for cat, n := range catCounts {
				parts = append(parts, fmt.Sprintf("%d %s", n, cat))
			}
			summary := fmt.Sprintf("%d memories consolidated (%s)", len(dbMemories), strings.Join(parts, ", "))
			if err := e.store.UpdateLearnedContext(reflectCtx, projectID, result.LearnedContext, summary); err != nil {
				e.logger.Error("save learned context", "error", err, "project_id", projectID)
			}
		}
	} else if result.LearnedContext != "" {
		// No memories but got learned context — update it.
		if err := e.store.UpdateLearnedContext(reflectCtx, projectID, result.LearnedContext, ""); err != nil {
			e.logger.Error("save learned context", "error", err, "project_id", projectID)
		}
	}

	e.logger.Info("reflection completed", "project_id", projectID, "interaction_count", count)
}

func parseReflectionResponse(text string) ReflectionResult {
	text = strings.TrimSpace(text)

	// Strip markdown code fences.
	if strings.HasPrefix(text, "```") {
		if idx := strings.Index(text, "\n"); idx != -1 {
			text = text[idx+1:]
		}
		if idx := strings.LastIndex(text, "```"); idx != -1 {
			text = text[:idx]
		}
		text = strings.TrimSpace(text)
	}

	var result ReflectionResult
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		return ReflectionResult{LearnedContext: text}
	}

	// Validate importance ranges.
	for i := range result.Memories {
		if result.Memories[i].Importance < 0 {
			result.Memories[i].Importance = 0
		}
		if result.Memories[i].Importance > 1 {
			result.Memories[i].Importance = 1
		}
		if result.Memories[i].Tags == nil {
			result.Memories[i].Tags = []string{}
		}
	}

	return result
}
