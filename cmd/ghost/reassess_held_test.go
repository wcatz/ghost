package main

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/resolve"
)

// #712: `ghost resolve <project> --reassess` printed a count of the rows it left
// resolved and nothing else, so an operator repairing a store could not tell
// which rows those were or what asserted each one. These are the two lines that
// answer it.

// TestReassessHeldLinesWithholdsCredentials: the held row's own text reaches a
// print site like every other one, and a stored credential must be withheld
// whole rather than quoted into a report an operator pastes into a shell.
func TestReassessHeldLinesWithholdsCredentials(t *testing.T) {
	got := reassessHeldLines([]resolve.HeldMemory{{
		Memory: memory.Memory{ID: "row", Category: "changelog",
			Content: "the deploy key AKIAIOSFODNN7EXAMPLE is in the vault"},
		Holds: []resolve.Hold{{Kind: resolve.HoldCorrection, Holder: "corr"}},
	}})
	if strings.Contains(got, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("reassessHeldLines() printed a stored credential:\n%s", got)
	}
	if !strings.Contains(got, "held by correction corr") {
		t.Errorf("reassessHeldLines() = %q, want the hold named even when the text is withheld", got)
	}
}

// TestReassessRoundsLine: the re-check is bounded, so the report has to say how
// many rounds it took and — when the bound stopped it with a row still changing
// — that the repair is short of its fixed point and a further pass may clear
// more. A run whose first re-check settled everything says nothing, because a
// silent line is the one an operator learns to skip.
func TestReassessRoundsLine(t *testing.T) {
	if got := reassessRoundsLine(resolve.ReassessResult{Rounds: 1}); got != "" {
		t.Errorf("reassessRoundsLine() on a one-round run = %q, want nothing to say", got)
	}
	got := reassessRoundsLine(resolve.ReassessResult{Rounds: 3})
	if !strings.Contains(got, "3 round") || !strings.Contains(got, "fixed point") {
		t.Errorf("reassessRoundsLine() = %q, want the round count and that it is a fixed point", got)
	}
	if strings.Contains(got, "further pass") {
		t.Errorf("reassessRoundsLine() = %q, want no further-pass warning on a run that settled", got)
	}
	bound := reassessRoundsLine(resolve.ReassessResult{Rounds: 8, BoundHit: true})
	if !strings.Contains(bound, "further pass may clear more") {
		t.Errorf("reassessRoundsLine() on the bound = %q, want it to say a further pass may clear more", bound)
	}
	if !strings.Contains(bound, "8 round") {
		t.Errorf("reassessRoundsLine() on the bound = %q, want the bound named with its count", bound)
	}
}
