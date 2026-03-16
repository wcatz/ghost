package tool

import "github.com/wcatz/ghost/internal/memory"

// RegisterAll registers all built-in tools.
// Read-only tools (file_read, grep, glob, git) let Ghost see the repo.
// Write tools (file_write, file_edit, bash) are NOT registered — Claude Code handles writing.
func RegisterAll(r *Registry, store *memory.Store) {
	registerFileRead(r)
	registerGrep(r)
	registerGlob(r)
	registerGit(r)
	registerMemorySave(r, store)
	registerMemorySearch(r, store)
}
