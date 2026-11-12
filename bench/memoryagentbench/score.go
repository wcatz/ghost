package main

import (
	"strings"

	"github.com/wcatz/ghost/internal/memory"
)

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
