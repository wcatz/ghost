package memory

import (
	"context"
	"testing"
)

// TestRecordAndRecentMaintenanceRuns: RecordMaintenanceRun inserts a row with
// a generated id and server-side timestamp; RecentMaintenanceRuns returns them
// newest-first with every field the status printer shows (scratch bytes,
// reaped counts, timestamps, note), honoring the limit.
func TestRecordAndRecentMaintenanceRuns(t *testing.T) {
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close() //nolint:errcheck
	ctx := context.Background()

	// Empty to begin with.
	runs, err := RecentMaintenanceRuns(ctx, db, 10)
	if err != nil {
		t.Fatalf("RecentMaintenanceRuns on empty db: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("empty db returned %d runs", len(runs))
	}

	first := MaintenanceRun{
		Kind:               "scratch-budget",
		ScratchBytes:       8192,
		ScratchReapedBytes: 8192,
		ScratchReapedCount: 1,
		Note:               "over budget by 4096 bytes; proceeding",
	}
	if err := RecordMaintenanceRun(ctx, db, first); err != nil {
		t.Fatalf("RecordMaintenanceRun: %v", err)
	}
	second := MaintenanceRun{Kind: "scratch-budget", ScratchBytes: 512, Note: "within budget after reaping stale entries"}
	if err := RecordMaintenanceRun(ctx, db, second); err != nil {
		t.Fatalf("RecordMaintenanceRun (second): %v", err)
	}

	runs, err = RecentMaintenanceRuns(ctx, db, 10)
	if err != nil {
		t.Fatalf("RecentMaintenanceRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("got %d runs, want 2", len(runs))
	}
	// Newest-first: the second insert leads.
	got := runs[0]
	if got.ScratchBytes != 512 || got.Kind != "scratch-budget" {
		t.Errorf("runs[0] = %+v, want the most recent row (512 B)", got)
	}
	if got.RecordedAt == "" {
		t.Errorf("RecordedAt empty — status must print a timestamp")
	}
	if got.ID == "" {
		t.Errorf("ID empty — rows must carry a generated id")
	}
	// Every field the first row stored survives the round-trip.
	firstRun := runs[1]
	if firstRun.ScratchBytes != 8192 || firstRun.ScratchReapedBytes != 8192 || firstRun.ScratchReapedCount != 1 {
		t.Errorf("first row round-trip = %+v, want scratch 8192 / reaped 8192 B × 1", firstRun)
	}
	if firstRun.Note != first.Note {
		t.Errorf("note = %q, want %q", firstRun.Note, first.Note)
	}

	// Limit is honored.
	runs, err = RecentMaintenanceRuns(ctx, db, 1)
	if err != nil {
		t.Fatalf("RecentMaintenanceRuns limit: %v", err)
	}
	if len(runs) != 1 {
		t.Errorf("limit=1 returned %d runs, want 1", len(runs))
	}
}
