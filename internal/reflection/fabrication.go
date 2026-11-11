package reflection

import (
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/wcatz/ghost/internal/secret"
)

var shaLikeRe = regexp.MustCompile(`(?i)\b[0-9a-f]{7,40}\b`)

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

func dropFabricatedMemories(result *ReflectionResult, input ReflectionInput, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	known := knownSHAs(input)

	kept := result.Memories[:0]
	for _, m := range result.Memories {
		unknown := unknownSHAs(m.Content, known)
		if len(unknown) == 0 {
			kept = append(kept, m)
			continue
		}
		logger.Warn("reflection dropped a memory with a fabricated commit SHA",
			"category", m.Category, "tokens", strings.Join(unknown, ","),
			"preview", previewContent(m.Content))
	}
	result.Memories = kept
}

func knownSHAs(input ReflectionInput) map[string]bool {
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
	return known
}

func unknownSHAs(text string, known map[string]bool) []string {
	var unknown []string
	for _, tok := range shaLikeRe.FindAllString(text, -1) {
		if shaLikeToken(tok) && !known[strings.ToLower(tok)] {
			unknown = append(unknown, tok)
		}
	}
	return unknown
}

func previewContent(content string) string {
	if finding, ok := secret.Detect(content); ok {
		return fmt.Sprintf("<withheld: %s, bytes=%d>", finding.Label, len(content))
	}
	const max = 160
	flat := strings.Join(strings.Fields(content), " ")
	if r := []rune(flat); len(r) > max {
		return string(r[:max]) + "…"
	}
	return flat
}
