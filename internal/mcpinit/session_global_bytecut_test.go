package mcpinit

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/assemble"
)

// TestGlobalHeadingKeptWhenEveryGlobalRowIsByteCut is the regression for #1837.
// When the response-fit pass cuts every global row for size, the global section
// used to disappear with nothing saying it was cut. The project half keeps its
// heading when tally.project.ByteCut > 0 even with zero rows shown; the global
// half must do the same, measured with the same Measure the fit pass uses.
func TestGlobalHeadingKeptWhenEveryGlobalRowIsByteCut(t *testing.T) {
	project := "ghost"
	learned := "A short summary."
	var tasks [][4]string
	var decisions [][4]string

	tally := sessionTally{
		project: assemble.BucketTally{Shown: 2, RankedOut: 10, ByteCut: 2},
		globals: assemble.BucketTally{Shown: 0, RankedOut: 8, ByteCut: 8},
	}

	block := formatSessionContext("p1", project, nil, nil, learned, tasks, decisions, 1, nil, tally)

	if !strings.Contains(block, "**Global (applies to all projects):**") {
		t.Fatalf("global heading must be kept when every global row is byte-cut; got:\n%s", block)
	}
	if !strings.Contains(block, "cut to keep this block under the host's output limit") {
		t.Fatalf("global counts line must carry the cut clause when globals are byte-cut; got:\n%s", block)
	}
	if !strings.Contains(block, "ghost_search_all") {
		t.Fatalf("global counts line must point at the tool that shows the rows; got:\n%s", block)
	}
	if len(block) > sessionStartByteCap {
		t.Fatalf("block is %d bytes, want at most %d (sessionStartByteCap) — the extra heading and counts line must count against the budget:\n%s", len(block), sessionStartByteCap, block)
	}
	if strings.Contains(block, "the user's own saved cross-project preferences") || strings.Contains(block, "cross-project memories from mixed origins") {
		t.Errorf("byte-cut global section must use the bucket label alone, not the origin-guidance sentence; got:\n%s", block)
	}
	// Also prove the converse: without ByteCut the heading is absent.
	emptyTally := sessionTally{
		project: assemble.BucketTally{Shown: 2, RankedOut: 10},
		globals: assemble.BucketTally{Shown: 0},
	}
	plain := formatSessionContext("p1", project, nil, nil, learned, tasks, decisions, 1, nil, emptyTally)
	if strings.Contains(plain, "**Global (applies to all projects):**") {
		t.Fatalf("global heading must not appear when no globals and no ByteCut; got:\n%s", plain)
	}
}
