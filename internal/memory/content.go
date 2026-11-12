package memory

import (
	"fmt"
	"unicode/utf8"
)

// MaxContentLen is the byte cap on content written by any production Ghost
// writer — the MCP save/update/global tools, consolidation (reflection)
const MaxContentLen = 8000

// MaxTitleLen is the byte cap on a TITLE — a task's, a decision's — and on one
// entry of a decision's alternatives list. It is the smaller, display-oriented
const MaxTitleLen = 300

// MaxAlternatives is the number of entries a decision's alternatives list is
// kept to, `ghost_decision_record`'s cap. `ghost_decisions_list` renders the
const MaxAlternatives = 20

// BoundAlternatives is the alternatives list a decision STORES: at most
// MaxAlternatives entries, in the order they were recorded, each cut at
func BoundAlternatives(alternatives []string) []string {
	if !AlternativesClamped(alternatives) {
		return alternatives
	}
	bounded := make([]string, 0, MaxAlternatives)
	for i, alt := range alternatives {
		if i == MaxAlternatives {
			break
		}
		bounded = append(bounded, TruncateUTF8(alt, MaxTitleLen))
	}
	return bounded
}

// AlternativesClamped reports whether BoundAlternatives would cut this list:
// more than MaxAlternatives entries, or any entry past MaxTitleLen.
func AlternativesClamped(alternatives []string) bool {
	if len(alternatives) > MaxAlternatives {
		return true
	}
	for _, alt := range alternatives {
		if len(alt) > MaxTitleLen {
			return true
		}
	}
	return false
}

// TruncationMarker returns the marker ClampContent appends to content it
// cut: it names the exact limit so the stored value itself shows how much
func TruncationMarker() string {
	return fmt.Sprintf(" …[truncated at %d bytes]", MaxContentLen)
}

// TruncateUTF8 returns the longest prefix of s that is at most maxBytes bytes
// and ends on a UTF-8 rune boundary. It does not append a marker; callers that
func TruncateUTF8(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	for maxBytes > 0 && !utf8.RuneStart(s[maxBytes]) {
		maxBytes--
	}
	return s[:maxBytes]
}

// ClampContent applies MaxContentLen to s. Content that fits is returned
// byte-identical with cut=false — no marker, no rewrite. Content over the
func ClampContent(s string) (string, bool) {
	if len(s) <= MaxContentLen {
		return s, false
	}
	return TruncateUTF8(s, MaxContentLen) + TruncationMarker(), true
}
