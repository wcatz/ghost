package reflection

import "strings"

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

	normalized := strings.NewReplacer("-", "_", " ", "_", "=", "_", ":", "_").Replace(lower)
	for _, key := range []string{"api_key", "apikey", "credential", "password", "secret", "token", "private_key", "bearer"} {
		if strings.Contains(normalized, key+"_") || strings.Contains(normalized, "_"+key+"_") || strings.HasSuffix(normalized, "_"+key) {
			return true
		}
	}
	return false
}
