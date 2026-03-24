package reflection

import "context"

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
