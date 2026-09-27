package reflection

import (
	"context"
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
	// Per-id operations, not a rewritten memory list (#639). An unreadable
	// operation, an unknown id, or a contradiction between two operations fails
	// the whole response: applying the readable half of it would rewrite the
	// corpus as if the model had said something it did not, and the tiered
	// consolidator has a deterministic tier to fall through to instead.
	resp, err := parseOpResponse(responseText)
	if err != nil {
		return ReflectionResult{}, err
	}
	result, err := executeOps(resp, input, h.log())
	if err != nil {
		return ReflectionResult{}, err
	}
	normalizeReflectMemories(&result)
	dropFabricatedMemories(&result, input, h.log())
	dropForeignProjectMemories(&result, input, h.log())
	dropSecretMemories(&result, h.log())
	return result, nil
}

// normalizeReflectMemories enforces what an LLM emission is allowed to claim
// about itself. It runs after the operations have been executed and is
// deliberately a separate function: these rules are the difference between a bad
// suggestion and a permanent, machine-made decision, and they deserve to be
// tested directly rather than only through a harness call.
//
// Under the per-id contract most of what arrives here is a stored row or a merge
// derived from stored rows, so these rules are a backstop rather than the main
// defence: an importance the model never states, a scope the classifier inferred,
// and a merge whose text is entirely the model's.
//
// The scope rule is the one that matters. The SQLite tier refuses to promote
// anything secret-looking, and justified the LLM tier's gap by saying the LLM
// tier's prompt excludes secrets — but a prompt is a request, not a guarantee, so the check
// has to be on the value the model actually returned. A global memory is
// replayed into every future session in every project: promoting a credential
// does not contain a leak, it takes one confined to a single project and
// widens it to all of them (issue #545).
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
		if m.Scope == "global" && looksLikeSecret(strings.ToLower(m.Content)) {
			m.Scope = "project"
		}
	}
}
