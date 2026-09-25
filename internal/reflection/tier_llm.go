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

// LlmConsolidator uses an LLM (a configured CLI harness — claude, opencode,
// codex, or goose — or a source-matched provider) for consolidation. Highest
// quality tier.
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
			snippet = memory.TruncateUTF8(snippet, 120) + "..."
		}
		return ReflectionResult{}, fmt.Errorf("reflection output is not valid JSON: %w (starts: %q)", err, snippet)
	}

	normalizeReflectMemories(&result)

	return result, nil
}

// normalizeReflectMemories enforces what an LLM emission is allowed to claim
// about itself. It runs after unmarshalling and is deliberately a separate
// function: these rules are the difference between a bad suggestion and a
// permanent, machine-made decision, and they deserve to be tested directly
// rather than only through a harness call.
//
// The scope rule is the one that matters. The SQLite tier refuses to promote
// anything secret-looking, and justified it by saying the LLM tier's prompt
// excludes secrets — but a prompt is a request, not a guarantee, so the check
// has to be on the value the model actually returned. A global memory is
// replayed into every future session in every project: promoting a credential
// does not contain a leak, it takes one confined to a single project and
// widens it to all of them.
func normalizeReflectMemories(result *ReflectionResult) {
	for i := range result.Memories {
		m := &result.Memories[i]
		if m.Importance < 0 {
			m.Importance = 0
		}
		if m.Importance > 1 {
			m.Importance = 1
		}
		if m.Tags == nil {
			m.Tags = []string{}
		}
		if m.Scope != "global" {
			m.Scope = "project"
		}
		// An invalid category would fail the schema CHECK inside
		// ReplaceNonManual and sink the whole apply transaction — fall back
		// to the schema default, the same way invalid scope collapses.
		if !memory.IsValidCategory(m.Category) {
			m.Category = "fact"
		}
		if m.Scope == "global" && looksLikeSecret(m.Content) {
			m.Scope = "project"
		}
	}
}
