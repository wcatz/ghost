package provider

// Compile-time interface satisfaction checks.
// These ensure that the concrete types implement the provider interfaces.
// They produce no runtime code — the compiler validates at build time.

import (
	"github.com/wcatz/ghost/internal/ai"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/prompt"
)

var (
	_ LLMProvider  = (*ai.Client)(nil)
	_ MemoryStore  = (*memory.Store)(nil)
	_ PromptBuilder = (*prompt.Builder)(nil)
)
