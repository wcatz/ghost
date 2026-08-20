package main

import (
	"strings"

	"github.com/wcatz/ghost/internal/memory"
)

// answerHit reports whether content contains any of answers as a
// case-insensitive substring — the same metric shape MemoryAgentBench's own
// leaderboard uses (substring_exact_match).
func answerHit(content string, answers []string) bool {
	lc := strings.ToLower(content)
	for _, a := range answers {
		if a == "" {
			continue
		}
		if strings.Contains(lc, strings.ToLower(a)) {
			return true
		}
	}
	return false
}

// topKHit reports whether any of the top k results is an answerHit.
func topKHit(results []memory.Memory, answers []string, k int) bool {
	if k > len(results) {
		k = len(results)
	}
	for i := 0; i < k; i++ {
		if answerHit(results[i].Content, answers) {
			return true
		}
	}
	return false
}
