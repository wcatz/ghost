package reflection

import (
	"log/slog"
	"strings"

	"github.com/wcatz/ghost/internal/secret"
)

// looksLikeSecret flags content that plausibly contains a credential. The
// lower-case input contract is shared by both reflection tiers: a global
// memory is replayed into every project, so neither heuristic classification
// nor an LLM response may widen a project-local secret into global context.
//
// It is a scope decision, not a detection, and it must stay that way. The words
// it matches on are the vocabulary of any project that handles credentials —
// "rotate the password quarterly", "the access token is in the runner env" — so
// refusing a save on it would refuse ordinary knowledge about secrets. What
// "is this an actual credential value" is a different and much narrower
// question, answered by internal/secret and enforced at the store layer (see
// internal/memory/secret_guard.go) and here.
func looksLikeSecret(lower string) bool {
	secretPatterns := []string{
		"api key", "api_key", "apikey", "credential", "password",
		"secret ", "secret_", "token ", "token_", "access_token",
		"private key", "bearer ",
	}
	for _, p := range secretPatterns {
		if strings.Contains(lower, p) {
			return true
		}
	}

	// Assignment syntax is common in operational notes and is easy to miss
	// when the key and separator are joined without a prose word ("TOKEN=...",
	// "CLIENT_SECRET:...", "api-key:..."). Normalize separators first, then
	// require a key boundary so ordinary words such as "tokenizer" do not
	// become global blockers.
	normalized := strings.NewReplacer("-", "_", " ", "_", "=", "_", ":", "_").Replace(lower)
	for _, key := range []string{"api_key", "apikey", "credential", "password", "secret", "token", "private_key", "bearer"} {
		if strings.Contains(normalized, key+"_") || strings.Contains(normalized, "_"+key+"_") || strings.HasSuffix(normalized, "_"+key) {
			return true
		}
	}
	return false
}

// dropSecretMemories removes emitted memories whose content holds a credential
// value, keeping the rest of the round.
//
// Dropping rather than failing is the whole point, and it is why this lives in
// the tier rather than relying on the store's refusal: ApplyReflection replaces
// the project corpus in one transaction, so a refusal from inside it would roll
// back a whole consolidation — every merge, every preserved row — over one
// hallucinated value. This is the same shape as the sibling guards
// (dropFabricatedMemories, dropForeignProjectMemories): one contaminated memory
// must not nuke an otherwise healthy pass.
//
// LearnedContext is deliberately not touched. It is a separate store write with
// its own failure mode, guarded at the store layer, where a refusal costs one
// unrecorded summary rather than a dropped memory.
//
// Every drop is logged, because a silently discarded memory is
// indistinguishable from one the model never emitted. The log names the format
// and the category and never the content: this line is the last place a
// credential would go before the log file keeps it forever.
//
// A nil logger becomes slog.Default(), the same substitution
// dropFabricatedMemories and dropForeignProjectMemories make. The reasoning is
// the guard's own: a drop that reports itself nowhere is a drop nobody can
// audit, and a call site that cannot log at all is a bug to fix loudly rather
// than to make silent.
func dropSecretMemories(result *ReflectionResult, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	kept := result.Memories[:0]
	for _, m := range result.Memories {
		finding, ok := secret.Detect(m.Content)
		if !ok {
			kept = append(kept, m)
			continue
		}
		logger.Warn("reflection memory dropped: content holds a credential value",
			"format", finding.Label,
			"category", m.Category,
			"content_bytes", len(m.Content))
	}
	result.Memories = kept
}
