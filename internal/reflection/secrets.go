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

// DropSecretMemories removes emitted memories whose content holds a credential
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
// It is called from cmd/ghost AFTER AuditGuardedDrops, not from inside the
// tier, and that ordering is load-bearing. The audit asks which inputs the
// output failed to account for; executeOps emits every unclaimed input verbatim,
// so a memory the tier carried forward is byte-identical to the output that
// carried it. Filtering earlier makes that input look unaccounted for, and both
// outcomes are wrong: RetainGuardedDrops re-adds it verbatim and the credential
// row is written back, so the guard is a no-op — after the audit has already
// printed its content to stderr, which is the same leak the store's refusal
// report avoids — or some other output happens to cover 45% of its tokens, the
// audit stays quiet, and ReplaceNonManual deletes the stored row with no
// --allow-drops, which is the one deletion path the drop guard exists to close.
//
// Run after the audit, a credential memory is still accounted for by the output
// that carried it, so it is neither re-added nor deleted: the row stays as it
// was, the value never enters the set that is written, and clearing a
// credential out of an existing database remains the separate, report-first scan
// rather than a side effect of consolidation.
//
// It returns how many memories it removed, so the caller can say so without
// re-running the scan.
//
// A nil logger becomes slog.Default(), the same substitution
// dropFabricatedMemories and dropForeignProjectMemories make. The reasoning is
// the guard's own: a drop that reports itself nowhere is a drop nobody can
// audit, and a call site that cannot log at all is a bug to fix loudly rather
// than to make silent.
func DropSecretMemories(result *ReflectionResult, logger *slog.Logger) int {
	if logger == nil {
		logger = slog.Default()
	}
	before := len(result.Memories)
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
	return before - len(kept)
}
