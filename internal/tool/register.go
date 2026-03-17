package tool

import "github.com/wcatz/ghost/internal/memory"

// RegisterAll registers all built-in tools.
func RegisterAll(r *Registry, store *memory.Store) {
	// Read tools — auto-approved, no user confirmation needed.
	registerFileRead(r)
	registerGrep(r)
	registerGlob(r)
	registerGit(r)

	// Write tools — require user confirmation (ApprovalWarn or ApprovalRequire).
	registerFileWrite(r)
	registerFileEdit(r)
	registerBash(r)

	// Memory tools.
	registerMemorySave(r, store)
	registerMemorySearch(r, store)
}
