package main

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestPrintPurgeReportMatchesTheCheckpointOutcome: the CLI's purge sentence is a
// claim about the file, and the checkpoint can fail to truncate the write-ahead
// log without an error. The report has to choose between "truncated" and "still
// in the log" from the outcome, and must always name the copies no purge can
// reach rather than claim the text has ceased to exist (#1043).
func TestPrintPurgeReportMatchesTheCheckpointOutcome(t *testing.T) {
	t.Run("checkpoint complete", func(t *testing.T) {
		var out strings.Builder
		if err := printPurgeReport(&out, memory.PurgeOutcome{Rows: 3, CheckpointComplete: true}); err != nil {
			t.Fatalf("printPurgeReport: %v", err)
		}
		got := out.String()
		if !strings.Contains(got, "write-ahead log truncated") {
			t.Errorf("a complete checkpoint is not reported as one:\n%s", got)
		}
		if strings.Contains(got, "could not be truncated") {
			t.Errorf("a complete checkpoint is reported as incomplete:\n%s", got)
		}
		assertPurgeReportCaveat(t, got)
	})

	t.Run("checkpoint incomplete", func(t *testing.T) {
		var out strings.Builder
		if err := printPurgeReport(&out, memory.PurgeOutcome{Rows: 3, CheckpointComplete: false}); err != nil {
			t.Fatalf("printPurgeReport: %v", err)
		}
		got := out.String()
		if !strings.Contains(got, "could not be truncated") || !strings.Contains(got, "stays in it until they close") {
			t.Errorf("an incomplete checkpoint is not reported as one:\n%s", got)
		}
		if strings.Contains(got, "log truncated") {
			t.Errorf("an incomplete checkpoint is reported as truncated:\n%s", got)
		}
		assertPurgeReportCaveat(t, got)
	})

	t.Run("failed write is returned", func(t *testing.T) {
		if err := printPurgeReport(&failingWriter{failOn: 1}, memory.PurgeOutcome{Rows: 3, CheckpointComplete: true}); err == nil {
			t.Fatal("a failed write was reported as a printed purge")
		}
	})
}

func assertPurgeReportCaveat(t *testing.T, got string) {
	t.Helper()
	for _, want := range []string{"older Ghost version", "without secure delete", "ghost backup", "another machine's store"} {
		if !strings.Contains(got, want) {
			t.Errorf("the purge report does not name %q, so it over-claims the reach:\n%s", want, got)
		}
	}
}
