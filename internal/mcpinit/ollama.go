package mcpinit

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/embedding"
)

func checkOllama(w io.Writer, cfg *config.Config, check func(ok bool, pass, fail string)) embedding.Reachability {
	if !cfg.Embedding.Enabled {
		_, _ = fmt.Fprintln(w, "  - embedding disabled in config (FTS-only search)")
		return embedding.Reachable
	}
	client := embedding.NewClient(cfg.Embedding.OllamaURL, cfg.Embedding.Model, cfg.Embedding.Dimensions)
	ctx := context.Background()
	switch client.Probe(ctx) {
	case embedding.Unreachable:
		check(false, "", fmt.Sprintf("Ollama unreachable at %s — embeddings paused", cfg.Embedding.OllamaURL))
		return embedding.Unreachable
	case embedding.Inconclusive:
		check(false, "", fmt.Sprintf("Ollama liveness undetermined at %s — embeddings paused (probe timed out, not an outage)", cfg.Embedding.OllamaURL))
		return embedding.Inconclusive
	}
	present, err := client.HasModel(ctx)
	if err != nil {
		check(false, "", fmt.Sprintf("Ollama model check failed: %v", err))
		return embedding.Reachable
	}
	check(present,
		fmt.Sprintf("Ollama model %s installed", cfg.Embedding.Model),
		fmt.Sprintf("Ollama model %s missing — run: ollama pull %s", cfg.Embedding.Model, cfg.Embedding.Model))
	return embedding.Reachable
}

const ollamaMarkerMax = 4096

func reportOllamaDownDuration(w io.Writer, dataDir string) {
	f, err := openMarker(filepath.Join(dataDir, embedding.OllamaDownMarkerFilename))
	if err != nil {
		return
	}
	defer f.Close() //nolint:errcheck
	data, err := io.ReadAll(io.LimitReader(f, ollamaMarkerMax+1))
	if err != nil || len(data) > ollamaMarkerMax {
		return
	}
	since, err := time.Parse(time.RFC3339, strings.TrimSpace(string(data)))
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "  ! Ollama down since %s (%s)\n",
		since.UTC().Format("2006-01-02 15:04 UTC"), formatDownDuration(time.Since(since)))
}

func formatDownDuration(d time.Duration) string {
	d = d.Truncate(time.Minute)
	days := d / (24 * time.Hour)
	d -= days * 24 * time.Hour
	hours := d / time.Hour
	d -= hours * time.Hour
	minutes := d / time.Minute

	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, minutes)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}
