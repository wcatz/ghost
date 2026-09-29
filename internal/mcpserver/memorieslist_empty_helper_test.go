package mcpserver

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/memory"
)

// newMemoriesListStore is a store with one registered project holding one row, so
// the test can assert both halves of the fix: an unknown name is refused, and a
// known one still lists what it holds.
func newMemoriesListStore(t *testing.T) *memory.Store {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	st := memory.NewStore(db, logger)
	// `_global` is created by seeding rather than by EnsureProject, and the global
	// row the test needs carries a foreign key onto it.
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('_global', '_global', 'global')`); err != nil {
		t.Fatalf("insert _global project: %v", err)
	}
	if err := st.EnsureProject(context.Background(), "mlist", t.TempDir(), "mlist"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if _, err := st.CreateWithIDFromCorpus(context.Background(), "mlist", "ownfact", memory.Memory{
		Category: "fact", Content: "a registered project's own fact", Source: "manual", Importance: 0.9,
	}); err != nil {
		t.Fatalf("seed project row: %v", err)
	}
	return st
}

// validityServerFor wraps a store in a Server and connects a client to it.
func validityServerFor(t *testing.T, st *memory.Store) (*Server, *mcp.ClientSession) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(st, logger, "test")
	return srv, connectedClient(t, srv)
}
