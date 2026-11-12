package mcpserver

import (
	"fmt"
	"regexp"
	"strings"
)

const repoFactAdvisory = " — ADVISORY: this reads as a repository fact — it names a file, a path or a symbol and says what is in it. " +
	"The repository is authoritative for that, so the note goes stale silently and is cheap to re-read from the code. " +
	"A memory is for durable knowledge: a rule, a constraint, a decision, or a reason the code does not state. " +
	"Stored anyway — Ghost guides saves, it never refuses one on a heuristic."

var repoFactReference = regexp.MustCompile(`(?:` +
	`\b[\w.-]*/[\w./-]*[\w-]+\.(?:go|mod|sum|py|pyi|ts|tsx|js|jsx|mjs|cjs|rs|java|kt|kts|rb|php|cs|c|h|cc|cpp|cxx|hpp|hxx|sh|bash|zsh|fish|sql|proto|ya?ml|toml|json|jsonc|ini|cfg|conf|env|tf|mk|cmake|gradle|lock)\b` +
	`|\b[a-z0-9][\w-]*\.(?:go|mod|sum|py|pyi|ts|tsx|js|jsx|mjs|cjs|rs|java|kt|kts|rb|php|cs|c|h|cc|cpp|cxx|hpp|hxx|sh|bash|zsh|fish|sql|proto|ya?ml|toml|json|jsonc|ini|cfg|conf|env|tf|mk|cmake|gradle|lock)\b` +
	`|\b(?:Makefile|Dockerfile|Rakefile|Gemfile|Caddyfile)\b` +
	`|\b[A-Za-z_][\w]*(?:\.[A-Za-z_][\w]*)*\(` +
	`|\b[a-z][\w]*\.[A-Z][\w]*\b` +
	`|\b[A-Za-z_][\w]*(?:\.[A-Za-z_][\w]*)*\.(?:[A-Z][a-z0-9]*[A-Z][\w]*|[A-Z]{2,})[\w]*` +
	`)`)

var repoFactPredicate = regexp.MustCompile(`(?i)\b(?:` +
	`contains?|defines?|defined|declares?|declared|implements?|implemented|named|` +
	`lives?|resides?|located|found|` +
	`is in|is at|is where|are in|are at|are where` +
	`)\b`)

var sentenceBoundary = regexp.MustCompile(`[.!?:;]\s|\n`)

var repoFactReason = regexp.MustCompile(`(?i)\b(?:` +
	`because|so that|on purpose|never|must` +
	`)\b`)

var repoFactRule = regexp.MustCompile(`(?i)\b(?:must|never|always)\b`)

var repoFactRuleCategory = map[string]bool{
	"convention": true,
	"decision":   true,
	"gotcha":     true,
}

func repoFactHint(content, category string) string {
	if repoFactReason.MatchString(content) {
		return ""
	}
	for _, sentence := range sentenceBoundary.Split(content, -1) {
		if !repoFactPredicate.MatchString(sentence) || !repoFactReference.MatchString(sentence) {
			continue
		}
		if repoFactRuleCategory[strings.ToLower(strings.TrimSpace(category))] && repoFactRule.MatchString(sentence) {
			continue
		}
		return repoFactAdvisory
	}
	return ""
}

// LongMemoryThreshold is the byte length above which a save draws the length advisory.
const LongMemoryThreshold = 800

const longMemoryAdvisoryFormat = " — ADVISORY: this memory is %d bytes (over the %d-byte threshold). Session start shows only ~300 chars of each memory, so only a fraction would appear. Save one rule and its reason per memory and split the rest into separate memories. Stored anyway."

func longMemoryHint(content string) string {
	if len(content) <= LongMemoryThreshold {
		return ""
	}
	return fmt.Sprintf(longMemoryAdvisoryFormat, len(content), LongMemoryThreshold)
}

// GlobalStatusDateScanBytes bounds how far into a global save the dated-status heuristic scans.
const GlobalStatusDateScanBytes = 120

var (
	isoDateRE              = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b`)
	attributedDateLeadRE   = regexp.MustCompile(`^(?:[^:\n]{1,60}?)(?:,\s*|\s+\()\d{4}-\d{2}-\d{2}\)?\s*:`)
	attributedPreferenceRE = regexp.MustCompile(`(?i)\b(?:always|never|must|prefer|prefers|preferred|rather avoid|avoid)\b`)
)

const globalStatusAdvisory = " — ADVISORY: this dated status line will be injected into every project's session start. Save it to its project instead, or set valid_until so it expires. Stored anyway — Ghost guides saves, it never refuses one on a heuristic."

func globalStatusHint(content string, validUntil *string) string {
	if validUntil != nil && strings.TrimSpace(*validUntil) != "" {
		return ""
	}
	trimmed := strings.TrimLeft(content, " \t\r\n")
	loc := isoDateRE.FindStringIndex(trimmed)
	if loc == nil || loc[0] >= GlobalStatusDateScanBytes {
		return ""
	}
	if loc[0] > 0 && attributedDateLeadRE.MatchString(trimmed) && attributedPreferenceRE.MatchString(trimmed) {
		return ""
	}
	return globalStatusAdvisory
}
