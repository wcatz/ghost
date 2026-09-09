package reflection

import (
	"regexp"
	"strings"
)

// shaLikeRe matches candidate git-SHA-shaped tokens: 7-40 hex chars,
// word-bounded. The 7-char floor is git's short-SHA default; 40 is a full
// SHA-1. shaLikeToken() then requires at least one digit, because pure-letter
// hex tokens ("defaced", "deadbeef") are English words, not SHAs — recorded
// fabrication failures have been mixed digit/letter tokens ("fdf4583",
// "0a1f004").
var shaLikeRe = regexp.MustCompile(`(?i)\b[0-9a-f]{7,40}\b`)

// shaLikeToken reports whether tok is a SHA-shaped token worth guarding:
// 7-40 hex chars containing at least one digit AND at least one a-f letter.
// The digit requirement excludes pure-letter English words ("defaced",
// "deadbeef"); the letter requirement excludes pure-digit numbers (network
// magics, ports) that are facts, not hashes.
func shaLikeToken(tok string) bool {
	if len(tok) < 7 || len(tok) > 40 {
		return false
	}
	hasDigit := false
	hasLetter := false
	for _, r := range tok {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
			hasLetter = true
		default:
			return false
		}
	}
	return hasDigit && hasLetter
}

// dropFabricatedMemories removes emitted memories whose content contains a
// SHA-shaped token that appears nowhere in the input corpus. A consolidation
// run cannot legitimately learn a new commit SHA — the model has no git
// access and LastCommits/ExistingMemories are the only sources — so any
// emitted SHA that isn't traceable to the input is a hallucinated specific
// (this repo has recorded bogus-SHA fabrications, e.g. "fdf4583" for a
// change actually made in 0a1f004). The memory is dropped rather than the
// whole run failing: one contaminated memory must not nuke an otherwise
// healthy consolidation pass.
func dropFabricatedMemories(result *ReflectionResult, input ReflectionInput) {
	known := make(map[string]bool)
	for _, m := range input.ExistingMemories {
		for _, tok := range shaLikeRe.FindAllString(m.Content, -1) {
			if shaLikeToken(tok) {
				known[strings.ToLower(tok)] = true
			}
		}
	}
	for _, c := range input.LastCommits {
		for _, tok := range shaLikeRe.FindAllString(c, -1) {
			if shaLikeToken(tok) {
				known[strings.ToLower(tok)] = true
			}
		}
	}

	kept := result.Memories[:0]
	for _, m := range result.Memories {
		suspect := false
		for _, tok := range shaLikeRe.FindAllString(m.Content, -1) {
			if shaLikeToken(tok) && !known[strings.ToLower(tok)] {
				suspect = true
				break
			}
		}
		if !suspect {
			kept = append(kept, m)
		}
	}
	result.Memories = kept
}
