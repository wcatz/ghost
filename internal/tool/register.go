package tool

import "github.com/wcatz/ghost/internal/memory"

// RegisterAll registers all built-in tools.
func RegisterAll(r *Registry, store *memory.Store) {
	registerFileRead(r)
	registerFileWrite(r)
	registerFileEdit(r)
	registerGrep(r)
	registerGlob(r)
	registerBash(r)
	registerGit(r)
	registerMemorySave(r, store)
	registerMemorySearch(r, store)
}
