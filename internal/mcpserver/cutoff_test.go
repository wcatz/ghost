package mcpserver

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
)

// The cutoff's one line of wiring (#954) is that the server's configured
// share reaches the assembler's query-mode stage. context.relevance_cutoff is a
// key a user sets believing it is in force; a value that binds in config and is
// dropped on the way to the request would be a silent no-op — the same failure
// TestTheConfiguredCosineReachesTheFloor pins for abstain_cosine. This asserts it
// at the tool boundary, against a real assemble.Run, so the whole config → server →
// assembler path is exercised rather than a field copy.

// cutoffRow is one candidate whose fused score the test controls, so the fixture
// can fall off where the cutoff will bite. It reuses weakRow's leg coverage
// (a keyword hit and an answered vector leg) so the answer is answerable, and
// moves only the score the cutoff compares.
func cutoffRow(id string, score float64) memory.Candidate {
	c := weakRow(id, "memory "+id)
	c.Score, c.Base = score, score
	return c
}

func TestTheConfiguredRelevanceCutoffReachesTheBlock(t *testing.T) {
	// A block whose scores fall off after the second row: the top two clear a 0.50
	// share of the top score, the bottom two do not.
	set := weakSet(
		cutoffRow("A1", 1.0),
		cutoffRow("A2", 0.9),
		cutoffRow("A3", 0.4),
		cutoffRow("A4", 0.3),
	)

	// Configured: the block stops where relevance falls off — two rows, the top
	// two, and the tail cut.
	on, onSession := newSearchSession(t, set)
	on.SetContextConfig(config.ContextConfig{RelevanceCutoff: 0.5})
	cut := searchText(t, onSession, "ledger")
	if got := countItemLines(cut); got != 2 {
		t.Errorf("cutoff-on admitted %d rows, want 2 (the top two above the 0.50 share):\n%s", got, tail(cut, 400))
	}
	for _, want := range []string{"memory A1", "memory A2"} {
		if !strings.Contains(cut, want) {
			t.Errorf("the block above the cutoff is missing %q:\n%s", want, tail(cut, 400))
		}
	}
	for _, gone := range []string{"memory A3", "memory A4"} {
		if strings.Contains(cut, gone) {
			t.Errorf("the block below the cutoff still carries %q: the answer did not stop where it fell off:\n%s", gone, tail(cut, 400))
		}
	}

	// Unconfigured (the shipped off state): the same four rows, because a machine
	// with no configured cutoff runs the historical answer, not a half-applied one.
	_, offSession := newSearchSession(t, set)
	off := searchText(t, offSession, "ledger")
	if got := countItemLines(off); got != 4 {
		t.Errorf("cutoff-off admitted %d rows, want all 4: an unconfigured cutoff must change nothing", got)
	}
}
