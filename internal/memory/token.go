package memory

import (
	"strconv"

	"github.com/wcatz/ghost/internal/secret"
)

// SafeToken renders a stored value for use outside data delimiters.
// It returns the value bare when all characters are in the safe set, otherwise as an ASCII-quoted string.
func SafeToken(s string) string {
	if s == "" {
		return `""`
	}
	for _, r := range s {
		if !IsSafeTokenRune(r) {
			return strconv.QuoteToASCII(s)
		}
	}
	return s
}

// IsSafeTokenRune reports whether r is in the set SafeToken writes bare.
func IsSafeTokenRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return r == '.' || r == '_' || r == '-' || r == ':' || r == '/' || r == '@' || r == '+'
}

// IsFormatRune reports whether r is a Unicode FORMAT character (category Cf).
func IsFormatRune(r rune) bool {
	return secret.IsFormatRune(r)
}
