package mcpserver

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestPurgeErasedTextReportsWhatTheCheckpointReached: the sentence a purge
// reports is a claim about the file, and wal_checkpoint(TRUNCATE) can fail to
// complete without an error. A message that said "erased" either way would be
// false for the incomplete case, and one that always hedged would be false for
// the complete one; the outcome has to choose. It must also never imply the
// purge reaches bytes an older build freed before secure delete was on, or a
// copy outside this database.
func TestPurgeErasedTextReportsWhatTheCheckpointReached(t *testing.T) {
	const lead = "Memory abc was deleted, and every version of its text this database recorded is gone."

	t.Run("checkpoint complete", func(t *testing.T) {
		got := purgeErasedText(lead, memory.PurgeOutcome{Rows: 2, CheckpointComplete: true})
		if !strings.Contains(got, "write-ahead log has been truncated") {
			t.Errorf("a complete checkpoint is not reported as one:\n%s", got)
		}
		if strings.Contains(got, "could not be truncated") {
			t.Errorf("a complete checkpoint is reported as incomplete:\n%s", got)
		}
		assertPurgeCaveat(t, got)
	})

	t.Run("checkpoint incomplete", func(t *testing.T) {
		got := purgeErasedText(lead, memory.PurgeOutcome{Rows: 2, CheckpointComplete: false})
		if !strings.Contains(got, "could not be truncated") {
			t.Errorf("an incomplete checkpoint is not reported as one:\n%s", got)
		}
		if !strings.Contains(got, "stays in it until they close") {
			t.Errorf("the incomplete report does not say the text stays in the log:\n%s", got)
		}
		if strings.Contains(got, "has been truncated") {
			t.Errorf("an incomplete checkpoint is reported as truncated:\n%s", got)
		}
		assertPurgeCaveat(t, got)
	})
}

// assertPurgeCaveat pins the two copies a purge never reaches, so a future edit
// cannot quietly promote the message into an unconditional "the text is gone".
func assertPurgeCaveat(t *testing.T, got string) {
	t.Helper()
	for _, want := range []string{"older Ghost version", "without secure delete", "ghost backup", "another machine's store"} {
		if !strings.Contains(got, want) {
			t.Errorf("the purge report does not name %q, so it over-claims the reach:\n%s", want, got)
		}
	}
}
