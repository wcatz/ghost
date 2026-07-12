package bench

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/linking"
)

// TestGraphConditionBuildsLinks self-verifies the hybrid+graph ablation: if the
// linking worker produced no links (e.g. threshold too high, or a future
// regression), hybrid+graph would silently equal hybrid and the ablation would
// measure nothing. Assert the graph is actually built on the committed dataset.
func TestGraphConditionBuildsLinks(t *testing.T) {
	ds, vecs := loadTestdataDataset(t)
	store := newBenchStore(t)
	ctx := context.Background()
	if _, err := Seed(ctx, store, ds, vecs); err != nil {
		t.Fatalf("seed: %v", err)
	}
	linking.NewWorker(store, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Hour, 0.70).SweepOnce(ctx)
	links, _, err := store.LinkStats(ctx)
	if err != nil {
		t.Fatalf("link stats: %v", err)
	}
	if links == 0 {
		t.Fatal("linking worker built no links at threshold 0.70; the hybrid+graph ablation is not exercising the graph bonus")
	}
	t.Logf("graph condition built %d links", links)
}
