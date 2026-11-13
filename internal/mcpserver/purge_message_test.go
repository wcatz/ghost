package mcpserver

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func TestPurgeErasedTextReportsWhatTheCheckpointReached(t *testing.T) {
	const lead = "Memory abc was deleted, and every version of its text this database recorded is gone."
	for _, tc := range []struct {
		name string
		out  memory.PurgeOutcome
		want []string
		no   []string
	}{
		{"checkpoint complete", memory.PurgeOutcome{Rows: 2, CheckpointComplete: true}, []string{"write-ahead log has been truncated"}, []string{"could not be truncated"}},
		{"checkpoint incomplete", memory.PurgeOutcome{Rows: 2, CheckpointComplete: false}, []string{"could not be truncated", "stays in it until they close"}, []string{"has been truncated"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := purgeErasedText(lead, tc.out)
			for _, w := range append(tc.want, "older Ghost version", "without secure delete", "ghost backup", "another machine's store") {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q:\n%s", w, got)
				}
			}
			for _, n := range tc.no {
				if strings.Contains(got, n) {
					t.Errorf("unexpected %q:\n%s", n, got)
				}
			}
		})
	}
}
