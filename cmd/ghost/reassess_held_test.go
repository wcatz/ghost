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

// TestReassessHeldLines: one line per held row, naming what holds it. The ids
// print WHOLE on these lines, not abbreviated like the per-memory listing beside
// them, because both are operands rather than references: the source id is what
// `ghost supersede --withdraw` takes, the held row's own id is what a scoped
// repair names, and the correction's id is the one an operator has to go and
// read in full. An eight-character abbreviation of any of them is a second
// lookup to discover a repair that was already in hand.
func TestReassessHeldLines(t *testing.T) {
	if got := reassessHeldLines(nil); got != "" {
		t.Errorf("reassessHeldLines(nil) = %q, want nothing to list", got)
	}
	held := []resolve.HeldMemory{
		{
			Memory: memory.Memory{ID: "a1a1a1a1b2b2c2c2d3d3e3e3f3f3a1a1", Category: "changelog",
				Content: "Cost estimate from May: $148/mo projected; actuals have since replaced it."},
			Holds: []resolve.Hold{{Kind: resolve.HoldSupersedes, Holder: "e5e5e5e5f6f6a6a6b7b7c7c7d7d7e5e5"}},
		},
		{
			Memory: memory.Memory{ID: "b2b2b2b2c3c3d3d3e4e4f4f4a4a4b2b2", Category: "gotcha",
				Content: "root cause: ledgerstate never sets CalculationVersion (closed)"},
			Holds: []resolve.Hold{{Kind: resolve.HoldCorrection, Holder: "c3c3c3c3d4d4e4e4f5f5a5a5b5b5c3c3"}},
		},
	}
	got := reassessHeldLines(held)
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("reassessHeldLines() printed %d line(s), want one per held row:\n%s", len(lines), got)
	}
	if !strings.HasPrefix(lines[0], "  a1a1a1a1b2b2c2c2d3d3e3e3f3f3a1a1  [changelog]  held by supersedes e5e5e5e5f6f6a6a6b7b7c7c7d7d7e5e5") {
		t.Errorf("line 0 = %q, want the held row's whole id, its category and the source of the edge", lines[0])
	}
	if !strings.Contains(lines[0], "Cost estimate from May") {
		t.Errorf("line 0 = %q, want the row's own text so the operator can recognise it", lines[0])
	}
	if !strings.HasPrefix(lines[1], "  b2b2b2b2c3c3d3d3e4e4f4f4a4a4b2b2  [gotcha]  held by correction c3c3c3c3d4d4e4e4f5f5a5a5b5b5c3c3") {
		t.Errorf("line 1 = %q, want the held row's whole id, its category and the correction that pairs it", lines[1])
	}
	// A row two corrections both pair is named by both, for the reason
	// resolve.HeldMemory.Reason states; the report must not collapse it back
	// to one, or the operator is sent to check a pairing that was not the only
	// one holding the row.
	both := reassessHeldLines([]resolve.HeldMemory{{
		Memory: memory.Memory{ID: "d4d4d4d4e5e5f5f5a6a6b6b6c6c6d4d4", Category: "changelog", Content: "shipped"},
		Holds: []resolve.Hold{
			{Kind: resolve.HoldSupersedes, Holder: "edge-one"},
			{Kind: resolve.HoldSupersedes, Holder: "edge-two"},
		},
	}})
	if !strings.Contains(both, "held by supersedes edge-one, supersedes edge-two") {
		t.Errorf("reassessHeldLines() = %q, want every holder named", both)
	}
}

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
