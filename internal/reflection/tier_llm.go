package reflection

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/wcatz/ghost/internal/ai"
	"github.com/wcatz/ghost/internal/memory"
)

// reflector is the subset of LLMProvider needed for LLM consolidation.
type reflector interface {
	Reflect(ctx context.Context, prompt string) (string, ai.TokenUsage, error)
}

// LlmConsolidator uses an LLM (a subscription-billed CLI harness — claude,
// opencode, codex, or goose — or a source-matched provider) for consolidation.
// Highest quality tier.
type LlmConsolidator struct {
	client reflector
	name   string
	logger *slog.Logger
}

// SetLogger lets the tiered consolidator route this tier's diagnostics through
// the configured sink (GHOST_LOG_FILE / level filtering) instead of the
// package-global default. Nil is the unset state; log() falls back.
func (h *LlmConsolidator) SetLogger(l *slog.Logger) { h.logger = l }

func (h *LlmConsolidator) log() *slog.Logger {
	if h.logger == nil {
		return slog.Default()
	}
	return h.logger
}

// NewLlmConsolidator wraps an existing LLM client that has a Reflect method.
// The tier reports its name as "llm".
func NewLlmConsolidator(client reflector) *LlmConsolidator {
	return &LlmConsolidator{client: client, name: "llm"}
}

// NewNamedConsolidator is NewLlmConsolidator with an explicit tier name —
// used when the caller wants Name() to report the concrete harness (e.g.
// "cli", "opencode", or a source name) rather than the generic "llm".
func NewNamedConsolidator(client reflector, name string) *LlmConsolidator {
	return &LlmConsolidator{client: client, name: name}
}

func (h *LlmConsolidator) Name() string { return h.name }

// Mechanical is false: this is an LLM tier, never exempt from the quality gate.
func (h *LlmConsolidator) Mechanical() bool { return false }

func (h *LlmConsolidator) Available(_ context.Context) bool {
	return h.client != nil
}

func (h *LlmConsolidator) Consolidate(ctx context.Context, input ReflectionInput) (ReflectionResult, error) {
	prompt := BuildReflectionPrompt(input)
	responseText, _, err := h.client.Reflect(ctx, prompt)
	if err != nil {
		return ReflectionResult{}, err
	}
	result, err := parseReflectionResponse(responseText)
	if err != nil {
		return ReflectionResult{}, err
	}
	dropFabricatedMemories(&result, input, h.log())
	dropForeignProjectMemories(&result, input, h.log())
	return result, nil
}

func parseReflectionResponse(text string) (ReflectionResult, error) {
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

	// Unparseable output is an error, not a result: the old fallback returned
	// the raw text as learned_context with ZERO memories, which read as "the
	// model consolidated everything away" — the tiered quality gate then fell
	// through to sqlite with no hint of the real cause (truncated/malformed JSON).
	var result ReflectionResult
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		snippet := text
		if len(snippet) > 120 {
			snippet = snippet[:120] + "..."
		}
		return ReflectionResult{}, fmt.Errorf("reflection output is not valid JSON: %w (starts: %q)", err, snippet)
	}

	// Validate importance ranges, scope, and category. An invalid category
	// would fail the schema CHECK inside ReplaceNonManual and sink the whole
	// apply transaction — normalize to the schema default ('fact') the same
	// way invalid scope collapses to 'project'.
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
		if !memory.IsValidCategory(result.Memories[i].Category) {
			result.Memories[i].Category = "fact"
		}
	}

	return result, nil
}
