package mcpinit

import (
	"regexp"
	"strings"
)

// directiveMaxLen bounds the text inspected for a standing-rule nudge.
const directiveMaxLen = 4096

var (
	directiveWeRE = regexp.MustCompile(`(?i)\bwe\s+(?:use|don't|dont|do not|won't|wont|will not|always|never)\b`)
	// Directive words anchored at the start or after sentence punctuation.
	directiveImperativeRE = regexp.MustCompile(`(?i)(?:^|[.!?]\s+)(?:please\s+|you\s+(?:should\s+|must\s+)?|we\s+(?:should\s+|must\s+)?)?(?:never|don't|dont|do not|always|stop|must)\b`)
)

// IsDirectivePrompt reports whether a user message states a standing rule.
// It is the one lexical check used by the working-moment hook and by the eval
// replay that measures how often that check fires on real messages.
func IsDirectivePrompt(prompt string) bool {
	s := strings.TrimSpace(prompt)
	if s == "" {
		return false
	}
	if len(s) > directiveMaxLen {
		s = s[:directiveMaxLen]
	}
	low := strings.ToLower(s)
	if strings.HasPrefix(low, "no,") {
		return true
	}
	if strings.HasPrefix(low, "that's not") || strings.HasPrefix(low, "thats not") || strings.HasPrefix(low, "that is not") {
		return true
	}
	if directiveWeRE.MatchString(s) {
		return true
	}
	if directiveImperativeRE.MatchString(s) {
		return true
	}
	return false
}

const directiveNudge = "Ghost: this prompt states a standing rule. Save it with ghost_memory_save (category preference or convention) before acting, and check whether an existing memory contradicts it."
