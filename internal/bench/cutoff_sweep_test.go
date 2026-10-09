package bench

import (
	"context"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/config"
)

// The cutoff sweep (#954) is the measurement the shipped default is chosen from,
// so it has to be a real function of the real block, not a hand-built table: it
// drives runContextAt (the same Store.Candidates -> assemble.Run path
// `ghost bench --context` measures) at every share of the parameter. This test is
// scoped to the small context fixture rather than the 220-query graded corpus,
// because a corpus-wide sweep is a corpus-wide block run per grid point and the
// bench package is within ~10s of CI's per-binary ceiling; the graded figures live
// in docs/benchmarks.md and `ghost bench --cutoff-sweep`, not in a test that
// re-runs them.
func TestContextCutoffSweepCoversTheGridAndRenders(t *testing.T) {
	store, queries, at := contextFixtureStore(t)
	grid := ContextCutoffGrid()
	points, err := ContextCutoffSweep(context.Background(), store, queries, at, grid)
	if err != nil {
		t.Fatalf("ContextCutoffSweep: %v", err)
	}
	if len(points) != len(grid) {
		t.Fatalf("sweep returned %d points, want one per grid entry (%d)", len(points), len(grid))
	}
	// The grid is walked in order, 0.0 first as the baseline a reader compares
	// against.
	if points[0].Cutoff != 0.0 {
		t.Errorf("first point cutoff = %.3f, want 0.000 (the baseline)", points[0].Cutoff)
	}
	// Every point measures the same queries, so the column is a gradient over one
	// population rather than a mix.
	for i, pt := range points {
		if pt.Report.Queries != points[0].Report.Queries {
			t.Errorf("point %d measured %d queries, want %d", i, pt.Report.Queries, points[0].Report.Queries)
		}
	}
	// The shipped default is IN the grid, so the chosen number is visible beside the
	// baseline rather than implied by an off-table constant.
	foundDefault := false
	for _, pt := range points {
		if pt.Cutoff == config.DefaultRelevanceCutoff {
			foundDefault = true
		}
	}
	if !foundDefault {
		t.Errorf("the grid (%.3v) does not hold the shipped default %.3f", grid, config.DefaultRelevanceCutoff)
	}

	out := FormatContextCutoffSweep(points)
	for _, want := range []string{"cutoff", "relevant", "context precision", "result rate", "ship gate", "<- shipped default"} {
		if !strings.Contains(out, want) {
			t.Errorf("the rendered sweep is missing %q:\n%s", want, out)
		}
	}
}
