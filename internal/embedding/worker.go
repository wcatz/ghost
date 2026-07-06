package embedding

import (
	"context"
	"log/slog"
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

// Worker periodically embeds memories that don't yet have vectors.
type Worker struct {
	client   *Client
	store    memoryStore
	logger   *slog.Logger
	interval time.Duration
}

// NewWorker creates a background embedding worker.
func NewWorker(client *Client, store memoryStore, logger *slog.Logger, interval time.Duration) *Worker {
	return &Worker{
		client:   client,
		store:    store,
		logger:   logger,
		interval: interval,
	}
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
			w.processProject(ctx, pid)

		case <-ticker.C:
			w.SweepOnce(ctx)
		}
	}
}

// SweepOnce embeds unembedded memories across all projects.
func (w *Worker) SweepOnce(ctx context.Context) {
	if !w.client.Alive(ctx) {
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
	if !w.client.Alive(ctx) {
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
			if !w.client.Alive(ctx) {
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
