package main

import "github.com/wcatz/ghost/internal/memory"

// truncateForDisplay shortens s to at most n bytes without splitting a
// multi-byte character. The marker is deliberately display-only; the stored
// memory keeps the full content unless the writer's content cap applies.
func truncateForDisplay(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return memory.TruncateUTF8(s, n) + "..."
}
