package embedding

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// memoryStore is the subset of provider.MemoryStore needed by the worker.
type memoryStore interface {
	ListProjects(ctx context.Context) ([]memory.Project, error)
	UnembeddedMemoryIDs(ctx context.Context, projectID string, limit int) ([]string, error)
	GetMemoryContent(ctx context.Context, id string) (string, error)
	StoreEmbedding(ctx context.Context, memoryID string, vec []float32, model string) error
}

// OllamaDownMarkerFilename is the sentinel file checkAlive writes into
// dataDir the first time it observes Ollama unreachable, and removes once
// Ollama answers reachable again. `ghost mcp status` (internal/mcpinit) reads
// it to report outage duration (see issue #287). It's a plain file rather
// than a ghost_state database column because Ollama connectivity is a single
// global infra fact, not project-scoped data, and because `ghost mcp status`
// runs as a separate, short-lived CLI process from the long-lived MCP server
// that hosts this worker — an in-process-only counter here would be
// invisible to it, so the fact has to be persisted to disk. The file holds an
// RFC3339 UTC timestamp of when Ollama was first observed down.
const OllamaDownMarkerFilename = "ollama-down-since"

// Worker periodically embeds memories that don't yet have vectors.
type Worker struct {
	client   *Client
	store    memoryStore
	logger   *slog.Logger
	interval time.Duration
	dataDir  string
}

// NewWorker creates a background embedding worker. dataDir is the ghost data
// directory (config.DataDir()) where the Ollama-down marker file (see
// OllamaDownMarkerFilename and checkAlive) is written and removed as
// reachability changes; pass "" to disable that bookkeeping entirely (e.g.
// tests that don't exercise it, or a caller whose own config.DataDir() call
// failed).
func NewWorker(client *Client, store memoryStore, logger *slog.Logger, interval time.Duration, dataDir string) *Worker {
	return &Worker{
		client:   client,
		store:    store,
		logger:   logger,
		interval: interval,
		dataDir:  dataDir,
	}
}

// checkAlive reports whether Ollama is currently reachable and, as a side
// effect, maintains the on-disk down-since marker (OllamaDownMarkerFilename)
// that `ghost mcp status` reads to report outage duration: the marker is
// written — only if one doesn't already exist, so a still-down Ollama
// doesn't keep resetting its own "down since" clock on every poll — the
// first time Alive() is observed false, and removed once Alive() is observed
// true again. A blank dataDir (set by callers that don't care about the
// marker) disables this bookkeeping and behaves exactly like calling
// client.Alive(ctx) directly. Best-effort: a failure to write or remove the
// marker is logged at debug level and never changes the reported liveness.
func (w *Worker) checkAlive(ctx context.Context) bool {
	alive := w.client.Alive(ctx)
	if w.dataDir == "" {
		return alive
	}

	markerPath := filepath.Join(w.dataDir, OllamaDownMarkerFilename)
	if alive {
		if err := os.Remove(markerPath); err != nil && !os.IsNotExist(err) {
			w.logger.Debug("embed: remove ollama-down marker", "error", err)
		}
		return true
	}

	if _, err := os.Stat(markerPath); os.IsNotExist(err) {
		ts := time.Now().UTC().Format(time.RFC3339)
		if err := os.WriteFile(markerPath, []byte(ts), 0o600); err != nil {
			w.logger.Debug("embed: write ollama-down marker", "error", err)
		}
	}
	return false
}

// Run starts the worker loop. Blocks until ctx is cancelled.
// Project IDs sent on the channel are processed immediately (new saves);
// the periodic sweep covers ALL projects so pre-existing memories backfill
// even when nothing is saved in the current session.
func (w *Worker) Run(ctx context.Context, projectIDs <-chan string) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case pid, ok := <-projectIDs:
			if !ok {
				return
			}
			w.safeProcessProject(ctx, pid)

		case <-ticker.C:
			w.safeSweepOnce(ctx)
		}
	}
}

// safeProcessProject wraps processProject with panic recovery. A panic here
// must not escape into Run's select loop: unwinding past Run itself would
// leave nothing left to fire the loop's next tick/message, so embedding
// would stop for good even after the panic is otherwise "handled". Recovering
// inside this small function means control returns to Run — still live —
// once this call completes, so the loop keeps going on the next iteration.
func (w *Worker) safeProcessProject(ctx context.Context, projectID string) {
	defer func() {
		if r := recover(); r != nil {
			w.logger.Error("panic in embedding processProject, recovered", "panic", r, "project_id", projectID)
		}
	}()
	w.processProject(ctx, projectID)
}

// safeSweepOnce wraps SweepOnce with panic recovery for the same reason as
// safeProcessProject above.
func (w *Worker) safeSweepOnce(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			w.logger.Error("panic in embedding sweep, recovered", "panic", r)
		}
	}()
	w.SweepOnce(ctx)
}

// SweepOnce embeds unembedded memories across all projects.
func (w *Worker) SweepOnce(ctx context.Context) {
	if !w.checkAlive(ctx) {
		return
	}
	projects, err := w.store.ListProjects(ctx)
	if err != nil {
		w.logger.Error("embed: list projects", "error", err)
		return
	}
	for _, p := range projects {
		if ctx.Err() != nil {
			return
		}
		w.processProject(ctx, p.ID)
	}
}

// EmbedOne embeds a single memory immediately. Useful after Create/Upsert.
func (w *Worker) EmbedOne(ctx context.Context, memoryID string) {
	content, err := w.store.GetMemoryContent(ctx, memoryID)
	if err != nil {
		w.logger.Debug("embed: get content", "error", err, "memory_id", memoryID)
		return
	}

	vec, err := w.client.Embed(ctx, content)
	if err != nil {
		w.logger.Debug("embed: ollama", "error", err, "memory_id", memoryID)
		return
	}

	if err := w.store.StoreEmbedding(ctx, memoryID, vec, w.client.model); err != nil {
		w.logger.Error("embed: store", "error", err, "memory_id", memoryID)
	}
}

func (w *Worker) processProject(ctx context.Context, projectID string) {
	// Check if Ollama is alive first.
	if !w.checkAlive(ctx) {
		return
	}

	ids, err := w.store.UnembeddedMemoryIDs(ctx, projectID, 50)
	if err != nil {
		w.logger.Error("embed: list unembedded", "error", err, "project_id", projectID)
		return
	}

	if len(ids) == 0 {
		return
	}

	w.logger.Info("embedding memories", "project_id", projectID, "count", len(ids))

	embedded, failed := 0, 0
	var lastErr error
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}

		content, err := w.store.GetMemoryContent(ctx, id)
		if err != nil {
			w.logger.Debug("embed: get content", "error", err, "memory_id", id)
			continue
		}

		vec, err := w.client.Embed(ctx, content)
		if err != nil {
			failed++
			lastErr = err
			w.logger.Debug("embed: ollama", "error", err, "memory_id", id)
			// If Ollama went down mid-batch, stop.
			if !w.checkAlive(ctx) {
				w.logger.Info("ollama unavailable, pausing embedding", "embedded", embedded)
				return
			}
			continue
		}

		if err := w.store.StoreEmbedding(ctx, id, vec, w.client.model); err != nil {
			w.logger.Error("embed: store", "error", err, "memory_id", id)
			continue
		}
		embedded++
	}

	// Surface persistent embed failures once per sweep — a missing Ollama
	// model otherwise fails silently at debug level forever.
	if failed > 0 {
		w.logger.Warn("embedding failures this sweep — check `ghost mcp status`",
			"project_id", projectID, "failed", failed, "embedded", embedded, "last_error", lastErr)
	}

	if embedded > 0 {
		w.logger.Info("embedding batch complete", "project_id", projectID, "embedded", embedded)
	}
}
