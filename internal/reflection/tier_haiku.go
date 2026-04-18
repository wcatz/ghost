package reflection

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/wcatz/ghost/internal/ai"
)

// reflector is the subset of LLMProvider needed for Haiku consolidation.
type reflector interface {
	Reflect(ctx context.Context, prompt string) (string, ai.TokenUsage, error)
}

// HaikuConsolidator uses the Anthropic API (Haiku model) for consolidation.
// Highest quality tier but requires API credits.
type HaikuConsolidator struct {
	client reflector
}

// NewHaikuConsolidator wraps an existing LLM client that has a Reflect method.
func NewHaikuConsolidator(client reflector) *HaikuConsolidator {
	return &HaikuConsolidator{client: client}
}

func (h *HaikuConsolidator) Name() string { return "haiku" }

func (h *HaikuConsolidator) Available(_ context.Context) bool {
	return h.client != nil
}

func (h *HaikuConsolidator) Consolidate(ctx context.Context, input ReflectionInput) (ReflectionResult, error) {
	prompt := BuildReflectionPrompt(input)
	responseText, _, err := h.client.Reflect(ctx, prompt)
	if err != nil {
		return ReflectionResult{}, err
	}
	return parseReflectionResponse(responseText), nil
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

	// Validate importance ranges and scope.
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
		if result.Memories[i].Scope != "global" {
			result.Memories[i].Scope = "project"
		}
	}

	return result
}
