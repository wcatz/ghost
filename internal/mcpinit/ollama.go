package mcpinit

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/embedding"
)

// checkOllama reports the configured embedding model's health — the
// embedding-disabled informational line, Ollama reachability, and model
// presence — through the check closure. Shared by Status, StatusOpencode, and
// RunOpencode so the logic and message strings live in one place. The caller
// feeds its own check closure; on failure it prints the fail text via that
// closure (e.g. Status's closure also flags the run unhealthy).
//
// The returned bool reports whether Ollama itself was actually probed and
// answered reachable: false both when embedding is disabled (never probed)
// and when the probe found it unreachable, true whenever Ollama answered —
// regardless of whether the configured model happens to be installed.
// checkStoreHealth uses it to gate the "Ollama down since" duration line onto
// a currently unreachable Ollama, and to suppress it while disabled.
func checkOllama(w io.Writer, cfg *config.Config, check func(ok bool, pass, fail string)) bool {
	if !cfg.Embedding.Enabled {
		_, _ = fmt.Fprintln(w, "  - embedding disabled in config (FTS-only search)")
		return false
	}
	client := embedding.NewClient(cfg.Embedding.OllamaURL, cfg.Embedding.Model, cfg.Embedding.Dimensions)
	ctx := context.Background()
	if !client.Alive(ctx) {
		check(false, "", fmt.Sprintf("Ollama unreachable at %s — embeddings paused", cfg.Embedding.OllamaURL))
		return false
	}
	present, err := client.HasModel(ctx)
	if err != nil {
		check(false, "", fmt.Sprintf("Ollama model check failed: %v", err))
		return true
	}
	check(present,
		fmt.Sprintf("Ollama model %s installed", cfg.Embedding.Model),
		fmt.Sprintf("Ollama model %s missing — run: ollama pull %s", cfg.Embedding.Model, cfg.Embedding.Model))
	return true
}

// reportOllamaDownDuration prints how long Ollama has been unreachable, based
// on the down-since marker file the embedding worker (internal/embedding.Worker)
// maintains at filepath.Join(dataDir, embedding.OllamaDownMarkerFilename) — see
// that constant's doc comment for why the fact is persisted to a plain file
// rather than a database column. It is purely informational, printed
// alongside (not instead of) checkOllama's own pass/fail line, and never
// fails the run itself: a missing or unparseable marker simply produces no
// output.
//
// The caller must only invoke this once it has independently confirmed via
// checkOllama's live probe that Ollama is *currently* unreachable. The marker
// can otherwise outlive an outage — it's only cleared the next time some
// worker instance's own Alive() probe observes Ollama reachable again, which
// never happens if that MCP server process has since exited — and printing a
// stale duration next to a passing Ollama check would contradict the check
// directly above it. This function stays a read-only reporter (it does not
// delete a stale marker itself even when it declines to print); the marker is
// harmless clutter until a future worker run clears it.
func reportOllamaDownDuration(w io.Writer, dataDir string) {
	data, err := os.ReadFile(filepath.Join(dataDir, embedding.OllamaDownMarkerFilename))
	if err != nil {
		return
	}
	since, err := time.Parse(time.RFC3339, strings.TrimSpace(string(data)))
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "  ! Ollama down since %s (%s)\n",
		since.UTC().Format("2006-01-02 15:04 UTC"), formatDownDuration(time.Since(since)))
}

// formatDownDuration renders d as a compact human string truncated to minute
// granularity: "2h 3m" style under a day, "3d 4h" once the outage spans a
// full day (minutes stop being useful precision at that point).
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
