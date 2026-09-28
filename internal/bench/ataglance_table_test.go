package bench

import (
	"os"
	"strings"
	"testing"
)

// TestBenchmarksDocAtAGlanceTableIsThreeColumns: the "At a glance" table is three
// columns, and in GitHub-flavoured Markdown a row continues until a blank line,
// with `|` as a cell delimiter and no padding required. So a paragraph pasted onto
// the end of the last row parses as a FOURTH cell, GFM drops the excess, and the
// paragraph disappears from the rendered page with no error anywhere — which is
// how "These rows are not one leaderboard…" was lost once already in this branch.
//
// Nothing else in the file catches that: a doc-drift test that reads a fenced
// code block is unaffected, and a text search for the paragraph finds it on the
// row. So the check is structural, on the shape the renderer sees.
func TestBenchmarksDocAtAGlanceTableIsThreeColumns(t *testing.T) {
	const doc = "../../docs/benchmarks.md"
	raw, err := os.ReadFile(doc)
	if err != nil {
		t.Fatalf("read %s: %v", doc, err)
	}
	lines := strings.Split(string(raw), "\n")

	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "| Evaluation |") {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("no '| Evaluation |' header in %s; the at-a-glance table is gone or renamed", doc)
	}

	rows := 0
	tableEnd := start
	for i := start; i < len(lines); i++ {
		l := lines[i]
		if !strings.HasPrefix(l, "|") {
			tableEnd = i // the table ends at the first non-row line
			break
		}
		if strings.HasPrefix(l, "|---") {
			continue
		}
		rows++
		// A three-column row splits into five fields: "", label, measure, result, "".
		if cells := strings.Split(l, "|"); len(cells) != 5 {
			t.Errorf("row %d has %d cells, so it is not a three-column row and GFM will drop the excess: %.100s",
				i+1, len(cells), l)
		}
		if !strings.HasSuffix(strings.TrimRight(l, " "), "|") {
			t.Errorf("row %d does not end at a `|`, so whatever follows it is read as another cell: %.100s", i+1, l)
		}
	}
	if rows < 8 {
		t.Errorf("found only %d at-a-glance rows; the scan probably stopped early", rows)
	}

	// The statement those rows are not comparable across has to be a PARAGRAPH in
	// THIS section, so it is its own line and not inside a cell. Scoped to the
	// section: a line anywhere else in the file that starts the same way would
	// satisfy an unbounded search, which is how the paragraph could be moved out
	// of the at-a-glance section and the test stay green — the same silent loss,
	// re-openable by an ordinary doc edit.
	const caveat = "These rows are not one leaderboard"
	sectionEnd := len(lines)
	for i := tableEnd; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") {
			sectionEnd = i
			break
		}
	}
	for i := tableEnd; i < sectionEnd; i++ {
		if !strings.Contains(lines[i], caveat) {
			continue
		}
		if !strings.HasPrefix(lines[i], caveat) {
			t.Fatalf("the not-one-leaderboard statement is mid-line at %d, so a renderer reads it as cell text: %.100s", i+1, lines[i])
		}
		return
	}
	t.Errorf("the at-a-glance section (%d..%d) no longer says the rows are not one leaderboard", tableEnd+1, sectionEnd)
}
