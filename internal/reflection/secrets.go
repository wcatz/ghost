package reflection

import "strings"

// looksLikeSecret flags content that plausibly contains a credential.
func looksLikeSecret(content string) bool {
	// Lower once here rather than at each call site: the label patterns are
	// case-insensitive, but the credential prefixes below are not. "AKIA" and
	// "AIza" are how those formats identify themselves, and lowercasing the
	// input first would make them unmatchable — which is the same blind spot
	// the label-only version had, one level down.
	lower := strings.ToLower(content)
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
	// Label-free credentials. Every pattern above needs a word — "token",
	// "password" — followed by a space, so a model that returns the credential
	// itself passes straight through: "sk-live-4eC39HqLyjWDarjtT1zdp7dc"
	// is a live Stripe key, and it contains none of them. A value-level guard
	// is the only thing that catches that shape, and for this caller a false
	// positive costs one memory staying project-scoped, while a false negative
	// widens a live credential into every project's injected context.
	//
	// The test is high-entropy text carrying a known credential prefix and a
	// long opaque body. Length and alphabet do most of the work: a secret is
	// long, has no spaces to break it into words, and mixes cases or digits far
	// more than prose does. Prose is filtered out by the same token count
	// requirement, so an ordinary sentence cannot reach the prefix check.
	return containsOpaqueCredential(content)
}
