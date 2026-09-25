package reflection

import "strings"

// looksLikeSecret flags content that plausibly contains a credential. The
// lower-case input contract is shared by both reflection tiers: a global
// memory is replayed into every project, so neither heuristic classification
// nor an LLM response may widen a project-local secret into global context.
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
	return false
}
