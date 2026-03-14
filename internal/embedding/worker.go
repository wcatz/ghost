package embedding

import (
	"context"
	"log/slog"
	"time"
)

// memoryStore is the subset of provider.MemoryStore needed by the worker.
type memoryStore interface {
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
func (w *Worker) Run(ctx context.Context, projectIDs <-chan string) {
	// Process any project ID sent to us immediately.
	// Also run a periodic sweep.
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	// Track active projects for periodic sweeps.
	projects := make(map[string]bool)

	for {
		select {
		case <-ctx.Done():
			return

		case pid, ok := <-projectIDs:
			if !ok {
				return
			}
			projects[pid] = true
			w.processProject(ctx, pid)

		case <-ticker.C:
			for pid := range projects {
				w.processProject(ctx, pid)
			}
		}
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

	embedded := 0
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

	if embedded > 0 {
		w.logger.Info("embedding batch complete", "project_id", projectID, "embedded", embedded)
	}
}
