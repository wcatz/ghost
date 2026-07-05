// Package linking discovers relationships between memories by comparing
// their embeddings, persisting them as memory_links edges. It follows the
// same self-healing lifecycle as embeddings: reflection may wipe and
// recreate memories at any time, and the periodic sweep relinks them.
package linking

import (
	"context"
	"log/slog"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// linkStore is the subset of memory.Store needed by the worker.
type linkStore interface {
	ListProjects(ctx context.Context) ([]memory.Project, error)
	UnscannedEmbeddedMemoryIDs(ctx context.Context, projectID string, limit int) ([]string, error)
	GetEmbedding(ctx context.Context, memoryID string) ([]float32, error)
	SearchVector(ctx context.Context, projectID string, queryVec []float32, limit int) ([]memory.ScoredMemory, error)
	CreateLink(ctx context.Context, sourceID, targetID, relation string, strength float32, source string) error
	MarkLinkScanned(ctx context.Context, memoryID string) error
}

// Worker periodically links embedded memories to their nearest neighbors.
type Worker struct {
	store     linkStore
	logger    *slog.Logger
	interval  time.Duration
	threshold float32
}

const (
	batchSize     = 50
	maxCandidates = 6
)

// NewWorker creates a background linking worker. Memories whose cosine
// similarity is at or above threshold get a 'related' link.
func NewWorker(store linkStore, logger *slog.Logger, interval time.Duration, threshold float32) *Worker {
	return &Worker{store: store, logger: logger, interval: interval, threshold: threshold}
}

// Run sweeps all projects on a ticker. Blocks until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.SweepOnce(ctx)
		}
	}
}

// SweepOnce links unscanned embedded memories across all projects.
func (w *Worker) SweepOnce(ctx context.Context) {
	projects, err := w.store.ListProjects(ctx)
	if err != nil {
		w.logger.Error("linking: list projects", "error", err)
		return
	}
	for _, p := range projects {
		if ctx.Err() != nil {
			return
		}
		w.processProject(ctx, p.ID)
	}
}

func (w *Worker) processProject(ctx context.Context, projectID string) {
	ids, err := w.store.UnscannedEmbeddedMemoryIDs(ctx, projectID, batchSize)
	if err != nil {
		w.logger.Error("linking: list unscanned", "error", err, "project_id", projectID)
		return
	}
	if len(ids) == 0 {
		return
	}

	linked := 0
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		vec, err := w.store.GetEmbedding(ctx, id)
		if err != nil {
			// Leave unscanned so the next sweep retries; errors here are
			// transient (deleted memories cascade out of the scan queue).
			w.logger.Debug("linking: get embedding", "error", err, "memory_id", id)
			continue
		}
		// +1 because the memory itself is its own nearest neighbor.
		candidates, err := w.store.SearchVector(ctx, projectID, vec, maxCandidates+1)
		if err != nil {
			w.logger.Debug("linking: search", "error", err, "memory_id", id)
			continue
		}
		failed := false
		for _, cand := range candidates {
			if cand.MemoryID == id || cand.Score < w.threshold {
				continue
			}
			if err := w.store.CreateLink(ctx, id, cand.MemoryID, "related", cand.Score, "auto"); err != nil {
				w.logger.Debug("linking: create link", "error", err, "memory_id", id)
				failed = true
				continue
			}
			linked++
		}
		// Only mark scanned when every link write succeeded, so missing
		// edges are retried on the next sweep.
		if failed {
			continue
		}
		if err := w.store.MarkLinkScanned(ctx, id); err != nil {
			w.logger.Error("linking: mark scanned", "error", err, "memory_id", id)
		}
	}
	if linked > 0 {
		// linked counts CreateLink writes; symmetric pairs normalize to one
		// row, so the distinct edge count can be lower.
		w.logger.Info("linking: batch complete", "project_id", projectID, "scanned", len(ids), "link_writes", linked)
	}
}
