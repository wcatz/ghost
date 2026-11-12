package mcpserver

import (
	"context"
	"fmt"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/provider"
)

type diagnosticReader interface {
	ListProjects(ctx context.Context) ([]memory.Project, error)
	CountMemories(ctx context.Context, projectID string) (int, error)
	CountConsolidatableMemories(ctx context.Context, projectID string) (int, error)
	EmbeddingStats(ctx context.Context) (embedded, stale, total int, err error)
	LinkStats(ctx context.Context) (links, scans int, err error)
	PinnedRowsWithContradictions(ctx context.Context, limit int) ([]memory.PinnedContradictedRow, int, error)
	HistoryGrowth(ctx context.Context) (memory.HistoryGrowthResult, error)
	GetAll(ctx context.Context, projectID string, limit int) ([]memory.Memory, error)
}

type diagnosticRead struct {
	reader   diagnosticReader
	store    *memory.Store
	reporter storeVersionReporter
	release  func()
}

func (d *diagnosticRead) end() {
	if d == nil || d.release == nil {
		return
	}
	d.release()
	d.release = nil
}

func (s *Server) openDiagnosticRead(ctx context.Context) *diagnosticRead {
	store, ok := s.store.(*memory.Store)
	if !ok {
		reporter, _ := s.store.(storeVersionReporter)
		return &diagnosticRead{reader: diagnosticStore{s.store}, reporter: reporter}
	}
	read, err := store.BeginDiagnosticRead(ctx)
	if err != nil {
		s.logger.Warn("diagnostic read unavailable; this report waits for an in-flight write", "error", err)
		return &diagnosticRead{reader: store, store: store, reporter: store}
	}
	if read == nil {
		return &diagnosticRead{reader: store, store: store, reporter: store}
	}
	view := read.Store()
	return &diagnosticRead{
		reader:   view,
		store:    view,
		reporter: view,
		release:  func() { _ = read.Close() },
	}
}

type diagnosticStore struct {
	provider.MemoryStore
}

func (d diagnosticStore) CountConsolidatableMemories(ctx context.Context, projectID string) (int, error) {
	return 0, fmt.Errorf("consolidatable count could not be read: store is not a *memory.Store")
}
