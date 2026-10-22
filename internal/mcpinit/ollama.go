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

// checkOllama reports the configured embedding model's health — the
// embedding-disabled informational line, Ollama reachability, and model
// presence — through the check closure. Shared by Status, StatusOpencode, and
// RunOpencode so the logic and message strings live in one place. The caller
// feeds its own check closure; on failure it prints the fail text via that
// closure (e.g. Status's closure also flags the run unhealthy).
//
// The returned Reachability is the verdict the probe reached, three-valued for
// the reason embedding.Probe is: Reachable when Ollama answered, Unreachable
// when the endpoint is conclusively not there (a refused connection or a
// non-200), and Inconclusive when the probe's own deadline expired without an
// answer. checkStoreHealth uses it to gate the "Ollama down since" duration
// line onto a CONCLUSIVE outage: a missed deadline establishes nothing, so it
// must never be rendered as an ongoing outage with a start time. The two
// non-Reachable verdicts print their own fail text — "unreachable" for the
// conclusive case, an undetermined endpoint for the timeout — and only the
// former licenses a duration beside it.
//
// Embedding disabled means nothing was probed, so the function returns before
// the client is built; the returned value carries no claim then, and the only
// caller that reads it (checkStoreHealth) gates the duration line on
// cfg.Embedding.Enabled as well, so a disabled config can never print an
// outage.
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
		// A missed deadline is not an outage: report the endpoint as slow or
		// undetermined so the reader does not take it for one.
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

// ollamaMarkerMax bounds the down-marker, which holds one RFC 3339 timestamp.
const ollamaMarkerMax = 4096

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
// checkOllama's live probe that Ollama is *conclusively* unreachable — an
// Unreachable verdict, never an Inconclusive one. Inconclusive means the
// probe's own deadline expired, which establishes nothing about the endpoint;
// treating it as an outage is how a currently-up-but-busy Ollama gets reported
// as hours down. A marker can otherwise outlive an outage — it's only cleared
// the next time some worker instance's own Alive() probe observes Ollama
// reachable again, which never happens if that MCP server process has since
// exited — and printing a stale duration next to a passing Ollama check would
// contradict the check directly above it. This function stays a read-only
// reporter (it does not delete a stale marker itself even when it declines to
// print); the marker is harmless clutter until a future worker run clears it.
func reportOllamaDownDuration(w io.Writer, dataDir string) {
	// openMarker, not os.ReadFile: the marker sits in the data dir beside files
	// any local process can plant, and a FIFO at its name would block `ghost mcp
	// status` in the open. A non-regular entry is no marker.
	f, err := openMarker(filepath.Join(dataDir, embedding.OllamaDownMarkerFilename))
	if err != nil {
		return
	}
	defer f.Close() //nolint:errcheck
	// One byte past the bound, so an oversized marker is ignored rather than
	// parsed from its first bytes.
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
