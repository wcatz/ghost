package mcpserver

// The diagnostic read path (#1738).
//
// ghost_health and ghost_list_projects are the two tools whose whole job is
// to report the store's state. A write in flight holds the store's mutex for
// its whole transaction, and the primary handle is pinned to one connection,
// so a report that reads through the primary handle waits for the write to
// commit — and a caller with a deadline gets a cancellation instead of a
// report.
//
// The two tools read through the store's read-only handle instead: a WAL
// reader sees the last committed state and never waits for a writer, so a
// report answers while the write it describes is still in flight.
//
// Every other read tool keeps the store's own reads. A reader that wants its
// own writes back — read-your-writes inside one tool call — has to stay on the
// primary handle, and the vector-cache epoch reads are bound to it.

import (
	"context"
	"fmt"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/provider"
)

// diagnosticReader is the read surface the diagnostic tools use.
//
// The store's own reads satisfy it (every one of these is on
// provider.MemoryStore), and so does the diagnostic view of a *memory.Store:
// the same readers, taken on the read handle. A tool is handed one or the
// other without knowing which it has.
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

// diagnosticRead is the read surface ONE diagnostic tool call reads through.
type diagnosticRead struct {
	// reader is where the reads go: the diagnostic view when the store has
	// one, the store's own reads otherwise.
	reader diagnosticReader
	// store is the memory store behind the reader, or nil when the server's
	// store is not one. The two blocks that need the concrete store — the
	// retrieval figures and the store-version line — need it.
	store *memory.Store
	// reporter is the store's version reporter, or nil when the store cannot
	// report one. It is the diagnostic view when there is one, so the version
	// is read on the same handle as the rest of the report.
	reporter storeVersionReporter
	// release ends the read: it rolls the read transaction back. nil when
	// there was no transaction to roll back.
	release func()
}

// end releases the read: it rolls the read transaction back, so the read
// handle's one connection is free again.
//
// It is safe to call more than once, so a caller can release the read as soon
// as its store statements are done and still defer the release for the paths
// that return early.
func (d *diagnosticRead) end() {
	if d == nil || d.release == nil {
		return
	}
	d.release()
	d.release = nil
}

// openDiagnosticRead returns the read surface a diagnostic tool call reads
// through.
//
// It is the store's read-only handle, bound to ONE read transaction, so every
// number in the report describes the same committed state and none of them
// waits for a write in flight.
//
// A store that cannot offer one — a store that is not a *memory.Store (a test
// double), or one built without NewStoreWithRead (an in-memory store, a bench
// store) — gets the store's own reads, which is what those stores have always
// used. The fallback is not a failure: there is no second handle to read
// through, and no second process to be blocked by.
//
// A read handle that cannot give a snapshot at all (a broken database, a
// refused read) falls back the same way, and says so in the log: the report is
// then as late as the write it waited for, which is what it was before, and a
// diagnostic that stops working when the store does is the wrong diagnostic.
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
		// No read handle: the store's own reads are the only ones there are.
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

// diagnosticStore adapts a provider.MemoryStore to diagnosticReader for the
// fallback path where the server's store is not a *memory.Store (a test double
// or a store built without a read handle). CountConsolidatableMemories is the
// one diagnostic reader method that is not on provider.MemoryStore, so it is
// implemented here by delegating to a non-*memory.Store test double and
// reporting an unreadable count otherwise.
type diagnosticStore struct {
	provider.MemoryStore
}

func (d diagnosticStore) CountConsolidatableMemories(ctx context.Context, projectID string) (int, error) {
	return 0, fmt.Errorf("consolidatable count could not be read: store is not a *memory.Store")
}
