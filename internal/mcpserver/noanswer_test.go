package mcpserver

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/config"
)

// The no-answer bar's one line of wiring (#955): the server's configured
// context.no_answer_cosine reaches the assembler's query-mode step. A key a user
// sets believing it is in force must not be dropped on the way to the request.
// weakRow carries a 0.44 cosine from an answered vector leg.
func TestTheConfiguredNoAnswerBarReachesTheBlock(t *testing.T) {
	set := weakSet(weakRow("A1", "memory A1"), weakRow("A2", "memory A2"))

	on, onSession := newSearchSession(t, set)
	on.SetContextConfig(config.ContextConfig{NoAnswerCosine: 0.5})
	got := searchText(t, onSession, "ledger")
	if n := countItemLines(got); n != 0 {
		t.Errorf("bar-on admitted %d rows, want none: the best cosine (0.44) is below 0.50:\n%s", n, tail(got, 400))
	}
	for _, want := range []string{"No memory answers this", "0.440", "0.500", "reason=nothing_cleared_the_bar"} {
		if !strings.Contains(got, want) {
			t.Errorf("the withheld answer is missing %q:\n%s", want, tail(got, 600))
		}
	}

	// A bar the best cosine clears leaves the block alone.
	clear, clearSession := newSearchSession(t, set)
	clear.SetContextConfig(config.ContextConfig{NoAnswerCosine: 0.40})
	if n := countItemLines(searchText(t, clearSession, "ledger")); n != 2 {
		t.Errorf("a bar below the best cosine admitted %d rows, want both", n)
	}

	// Unconfigured (the off state): the rows come back, as before the rule existed.
	_, offSession := newSearchSession(t, set)
	off := searchText(t, offSession, "ledger")
	if n := countItemLines(off); n != 2 {
		t.Errorf("bar-off admitted %d rows, want both: an unconfigured bar must change nothing", n)
	}
	if strings.Contains(off, "No memory answers this") || strings.Contains(off, "nothing_cleared_the_bar") {
		t.Errorf("the off state mentions the no-answer rule:\n%s", tail(off, 400))
	}
}
