package mcpinit

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
)

// measurePassiveStart runs the session-start block through the ASSEMBLER against
// the same store, with the policies the shipped loaders use, so the measurement
// covers the code this migration adds rather than only the code it leaves alone.
//
// The two numbers answer different questions and both belong in the PR body: the
// loader is what ships today, and the passive assembler path is what the hook
// switch will ship. Measuring only the loader would say nothing about whether the
// new path reads the whole store.
func measurePassiveStart(tb testing.TB, projectID string, now time.Time) (time.Duration, []string) {
	tb.Helper()
	dataDir, err := config.DataDir()
	if err != nil {
		tb.Fatalf("DataDir: %v", err)
	}
	db, err := memory.OpenReadDB(filepath.Join(dataDir, "ghost.db"))
	if err != nil {
		tb.Fatalf("OpenReadDB: %v", err)
	}
	defer db.Close() //nolint:errcheck
	store := memory.NewStoreWithRead(db, db, slog.New(slog.NewTextHandler(io.Discard, nil)))

	cfg := config.LoadForHook()
	inj := cfg.Injection
	start := time.Now()
	res, err := assemble.Run(context.Background(), store, assemble.Request{
		ProjectID: projectID,
		Query:     "", // passive: the shape a session start has
		Source:    assemble.SourceSessionStart,
		Condition: assemble.CondHybrid,
		Now:       now,
		Scope:     inj.SessionScope,
		Budget: assemble.Budget{Slices: []assemble.Slice{
			{
				Bucket: projectID, MaxItems: sessionMemoriesCap, OverFetch: sessionMemoriesCap * 3,
				ClampBytes: 200, Order: "decay", TwoPass: inj.BehaviorFloor > 0,
				BehaviorFloor: inj.BehaviorFloor, BehaviorCategories: inj.BehaviorCategories,
				CategoryWeights: inj.CategoryWeights, CategoryCaps: inj.CategoryCaps,
				DemotionThreshold: cfg.Linking.DemotionThreshold, DemoteOnlyWhenOverCap: true,
			},
			{
				Bucket: "_global", MaxItems: globalsCap, OverFetch: globalsCap * 2,
				ClampBytes: 300, Order: "pinned_importance_updated",
				DemotionThreshold: globalsDemotionThreshold, DropDemotedLosers: true,
			},
		}},
	})
	elapsed := time.Since(start)
	if err != nil {
		tb.Fatalf("passive assemble.Run: %v", err)
	}
	ids := make([]string, len(res.Items))
	for i, it := range res.Items {
		ids[i] = it.ID
	}
	return elapsed, ids
}

// TestPassiveAssemblerLoadDoesNotScaleWithStoreSize is the ratio check against
// the NEW path, and unlike the loader version in sessionstart_latency_test.go it
// can fail for an implementation of passive retrieval: it runs the block through
// `assemble.Run` with the shipped policies, so a passive fetch that stopped
// honouring its over-fetch would show up here as growth with store size.
func TestPassiveAssemblerLoadDoesNotScaleWithStoreSize(t *testing.T) {
	if testing.Short() {
		t.Skip("latency measurement: skipped in -short")
	}
	now := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)

	run := func(n int) (time.Duration, int) {
		xdgHome := t.TempDir()
		t.Setenv("XDG_DATA_HOME", xdgHome)
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		largeSessionStore(t, xdgHome, n)
		for i := 0; i < 2; i++ {
			measurePassiveStart(t, "pperf", now)
		}
		var total time.Duration
		var admitted int
		for i := 0; i < 20; i++ {
			d, ids := measurePassiveStart(t, "pperf", now)
			total += d
			admitted = len(ids)
		}
		return total / 20, admitted
	}

	small, smallAdmitted := run(100)
	large, largeAdmitted := run(1000)
	t.Logf("passive assemble.Run: 100 memories %s (%d admitted), 1000 memories %s (%d admitted), ratio %.2fx for 10x the rows",
		small, smallAdmitted, large, largeAdmitted, float64(large)/float64(small))

	if small <= 0 {
		t.Fatalf("implausible measurement at 100 memories: %s", small)
	}
	// The caps are the surface's contract, so an admitted count that grows with
	// the store is the same bug the timing measures, asserted directly.
	if smallAdmitted > sessionMemoriesCap+globalsCap || largeAdmitted > sessionMemoriesCap+globalsCap {
		t.Errorf("the passive block admitted %d (100 memories) and %d (1000) rows; the caps bound it at %d",
			smallAdmitted, largeAdmitted, sessionMemoriesCap+globalsCap)
	}
	if ratio := float64(large) / float64(small); ratio > 5 {
		t.Errorf("the passive assembler load grew %.2fx for a 10x larger store, which is the whole-store read this path must not do "+
			"(100 memories %s, 1000 memories %s)", ratio, small, large)
	}
}
