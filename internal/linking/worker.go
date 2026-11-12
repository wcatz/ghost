// Package linking discovers relationships between memories by comparing embeddings.
package linking

import (
	"context"
	"log/slog"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

type linkStore interface {
	ListProjects(ctx context.Context) ([]memory.Project, error)
	UnscannedEmbeddedMemoryIDs(ctx context.Context, projectID string, limit int) ([]string, error)
	GetEmbedding(ctx context.Context, memoryID string) ([]float32, error)
	GetByIDs(ctx context.Context, ids []string) ([]memory.Memory, error)
	SearchVectorScoped(ctx context.Context, projectID string, queryVec []float32, limit int, scope map[string]string) ([]memory.ScoredMemory, error)
	CreateLink(ctx context.Context, sourceID, targetID, relation string, strength float32, source string) error
	MarkLinkScanned(ctx context.Context, memoryID string, scanned []float32) error
	TrimOverCapRelatedDegrees(ctx context.Context) error
}

// Worker periodically links embedded memories to nearest neighbors.
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

// NewWorker creates a background linking worker.
func NewWorker(store linkStore, logger *slog.Logger, interval time.Duration, threshold float32) *Worker {
	return &Worker{store: store, logger: logger, interval: interval, threshold: threshold}
}

// Run sweeps projects on a ticker until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.safeSweepOnce(ctx)
		}
	}
}

func (w *Worker) safeSweepOnce(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			w.logger.Error("panic in linking sweep, recovered", "panic", r)
		}
	}()
	w.SweepOnce(ctx)
}

// SweepOnce links unscanned embedded memories across all projects.
func (w *Worker) SweepOnce(ctx context.Context) {
	if err := w.store.TrimOverCapRelatedDegrees(ctx); err != nil {
		w.logger.Error("linking: trim over-cap related degrees", "error", err)
	}
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
		w.logger.Error("linking: list unscanned", "error", err, "project_id", memory.RefusalArg("project_id", projectID))
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
			w.logger.Debug("linking: get embedding", "error", err, "memory_id", memory.RefusalArg("memory_id", id))
			continue
		}
		if len(vec) == 0 {
			w.logger.Debug("linking: no usable vector yet", "memory_id", memory.RefusalArg("memory_id", id))
			continue
		}
		sourceMemories, err := w.store.GetByIDs(ctx, []string{id})
		if err != nil {
			w.logger.Debug("linking: get source scope", "error", err, "memory_id", memory.RefusalArg("memory_id", id))
			continue
		}
		if len(sourceMemories) != 1 {
			w.logger.Debug("linking: source memory disappeared", "memory_id", memory.RefusalArg("memory_id", id))
			continue
		}
		sourceScope := sourceMemories[0].Scope
		// +1 because the memory itself is its own nearest neighbor.
		candidates, err := w.store.SearchVectorScoped(ctx, projectID, vec, maxCandidates+1, sourceScope)
		if err != nil {
			w.logger.Debug("linking: search", "error", err, "memory_id", memory.RefusalArg("memory_id", id))
			continue
		}
		failed := false
		for _, cand := range candidates {
			if cand.MemoryID == id || cand.Score < w.threshold {
				continue
			}
			if memory.ScopesConflict(sourceScope, cand.Scope) {
				continue
			}
			if err := w.store.CreateLink(ctx, id, cand.MemoryID, "related", cand.Score, "auto"); err != nil {
				w.logger.Debug("linking: create link", "error", err, "memory_id", memory.RefusalArg("memory_id", id))
				failed = true
				continue
			}
			linked++
		}
		if failed {
			continue
		}
		if err := w.store.MarkLinkScanned(ctx, id, vec); err != nil {
			w.logger.Error("linking: mark scanned", "error", err, "memory_id", memory.RefusalArg("memory_id", id))
		}
	}
	if linked > 0 {
		w.logger.Info("linking: batch complete", "project_id", memory.RefusalArg("project_id", projectID), "scanned", len(ids), "link_writes", linked)
	}
}
