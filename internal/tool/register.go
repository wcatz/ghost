package tool

import "github.com/wcatz/ghost/internal/memory"

// RegisterAll registers all built-in tools.
func RegisterAll(r *Registry, store *memory.Store) {
	registerMemorySave(r, store)
	registerMemorySearch(r, store)
}
