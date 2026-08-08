package mcpinit

import (
	"context"
	"fmt"
	"io"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/embedding"
)

// checkOllama reports the configured embedding model's health — the
// embedding-disabled informational line, Ollama reachability, and model
// presence — through the check closure. Shared by Status, StatusOpencode, and
// RunOpencode so the logic and message strings live in one place. The caller
// feeds its own check closure; on failure it prints the fail text via that
// closure (e.g. Status's closure also flags the run unhealthy).
func checkOllama(w io.Writer, cfg *config.Config, check func(ok bool, pass, fail string)) {
	if !cfg.Embedding.Enabled {
		_, _ = fmt.Fprintln(w, "  - embedding disabled in config (FTS-only search)")
		return
	}
	client := embedding.NewClient(cfg.Embedding.OllamaURL, cfg.Embedding.Model, cfg.Embedding.Dimensions)
	ctx := context.Background()
	if !client.Alive(ctx) {
		check(false, "", fmt.Sprintf("Ollama unreachable at %s — embeddings paused", cfg.Embedding.OllamaURL))
		return
	}
	present, err := client.HasModel(ctx)
	if err != nil {
		check(false, "", fmt.Sprintf("Ollama model check failed: %v", err))
		return
	}
	check(present,
		fmt.Sprintf("Ollama model %s installed", cfg.Embedding.Model),
		fmt.Sprintf("Ollama model %s missing — run: ollama pull %s", cfg.Embedding.Model, cfg.Embedding.Model))
}
