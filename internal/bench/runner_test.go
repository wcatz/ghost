package bench

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// seedRunnerStore builds an in-memory store with three well-separated memories
// and dim-3 hand vectors, returning the store and the relevant memory's ID.
func seedRunnerStore(t *testing.T) (*memory.Store, string) {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	store := memory.NewStore(db, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "p", "/tmp/p", "p"); err != nil {
		t.Fatal(err)
	}

	seed := []struct {
		content string
		vec     []float32
	}{
		{"kubernetes deployment alpha", []float32{1, 0, 0}},
		{"postgres database beta", []float32{0, 1, 0}},
		{"grafana dashboard gamma", []float32{0, 0, 1}},
	}
	var wantID string
	for i, s := range seed {
		id, err := store.Create(ctx, "p", memory.Memory{Category: "fact", Content: s.content, Importance: 0.7, Source: "mcp"})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.StoreEmbedding(ctx, id, s.vec, "test"); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			wantID = id // the "kubernetes" memory
		}
	}
	return store, wantID
}
