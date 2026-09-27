package reflection

import "strings"

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
