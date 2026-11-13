package mcpserver

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
)

// The cutoff's one line of wiring (#954) is that the server's configured

func cutoffRow(id string, score float64) memory.Candidate {
	c := weakRow(id, "memory "+id)
	c.Score, c.Base = score, score
	return c
}

func TestTheConfiguredRelevanceCutoffReachesTheBlock(t *testing.T) {
	set := weakSet(
		cutoffRow("A1", 1.0),
		cutoffRow("A2", 0.9),
		cutoffRow("A3", 0.4),
		cutoffRow("A4", 0.3),
	)

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

	_, offSession := newSearchSession(t, set)
	off := searchText(t, offSession, "ledger")
	if got := countItemLines(off); got != 4 {
		t.Errorf("cutoff-off admitted %d rows, want all 4: an unconfigured cutoff must change nothing", got)
	}
}
